package httpserver

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"buddy/server/internal/protocol"
	"buddy/server/internal/recording"
	"buddy/server/internal/store"
)

// fakeSessionStore is an in-memory store.Store for sessionDeleteHandler
// tests — real SQL correctness (transactional two-table delete, per-user
// scoping) is covered by internal/store's own container-backed tests.
type fakeSessionStore struct {
	deleted []struct{ userID, sessionID string }
	err     error
}

func (f *fakeSessionStore) Load(ctx context.Context, userID, sessionID string) (store.Profile, error) {
	return store.Profile{}, errors.New("not used by these tests")
}

func (f *fakeSessionStore) Save(ctx context.Context, userID, sessionID string, p store.Profile) error {
	return errors.New("not used by these tests")
}

func (f *fakeSessionStore) SaveTurn(ctx context.Context, userID, sessionID string, turn int, role, text string, refined bool) error {
	return errors.New("not used by these tests")
}

func (f *fakeSessionStore) SaveCorrection(ctx context.Context, userID, sessionID string, turn int, c protocol.Correction) error {
	return errors.New("not used by these tests")
}

func (f *fakeSessionStore) ListSessions(ctx context.Context, userID string) ([]store.SessionMeta, error) {
	return nil, errors.New("not used by these tests")
}

func (f *fakeSessionStore) SessionDetail(ctx context.Context, userID, sessionID string) (store.SessionMeta, []store.Turn, error) {
	return store.SessionMeta{}, nil, errors.New("not used by these tests")
}

func (f *fakeSessionStore) DeleteSession(ctx context.Context, userID, sessionID string) error {
	if f.err != nil {
		return f.err
	}
	f.deleted = append(f.deleted, struct{ userID, sessionID string }{userID, sessionID})
	return nil
}

func (f *fakeSessionStore) Close() error { return nil }

// fakeAudioBackupStore is an in-memory transport.AudioSaver for
// sessionDeleteHandler tests — real S3 behavior is covered by
// internal/audiostore's own tests.
type fakeAudioBackupStore struct {
	byUser map[string][]string // userID -> keys, formatted "sessionID/objectID"
	err    error
}

func (f *fakeAudioBackupStore) SaveStream(ctx context.Context, key string, r io.Reader) error {
	return errors.New("not used by these tests")
}

// DeleteBySession removes every key recorded under (userID, sessionID) —
// used by the cascading-delete handler tests.
func (f *fakeAudioBackupStore) DeleteBySession(ctx context.Context, userID, sessionID string) error {
	if f.err != nil {
		return f.err
	}
	out := f.byUser[userID][:0]
	for _, key := range f.byUser[userID] {
		if !strings.HasPrefix(key, sessionID+"/") {
			out = append(out, key)
		}
	}
	f.byUser[userID] = out
	return nil
}

func TestSessionDeleteRemovesTheSession(t *testing.T) {
	st := &fakeSessionStore{}
	h := sessionDeleteHandler(fakeIdentifier{id: "alex", ok: true}, st, nil, nil)

	req := httptest.NewRequest("DELETE", "/api/sessions/s1", nil)
	req.SetPathValue("id", "s1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	if len(st.deleted) != 1 || st.deleted[0].userID != "alex" || st.deleted[0].sessionID != "s1" {
		t.Fatalf("deleted = %+v, want exactly one (alex, s1)", st.deleted)
	}
}

func TestSessionDeleteUnauthorizedWhenIdentifyFails(t *testing.T) {
	h := sessionDeleteHandler(fakeIdentifier{ok: false}, &fakeSessionStore{}, nil, nil)

	req := httptest.NewRequest("DELETE", "/api/sessions/s1", nil)
	req.SetPathValue("id", "s1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestSessionDeleteInternalErrorOnStoreFailure(t *testing.T) {
	st := &fakeSessionStore{err: errors.New("mysql unreachable")}
	h := sessionDeleteHandler(fakeIdentifier{id: "alex", ok: true}, st, nil, nil)

	req := httptest.NewRequest("DELETE", "/api/sessions/s1", nil)
	req.SetPathValue("id", "s1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

// TestSessionDeleteCascadesToRecordings is the key behavior this handler
// adds beyond a plain session delete: deleting a chat room must also remove
// every recording archived under it (see recording.Store.DeleteBySession),
// so a learner deleting a conversation doesn't leave orphaned audio in S3.
func TestSessionDeleteCascadesToRecordings(t *testing.T) {
	st := &fakeSessionStore{}
	recStore := &fakeRecordingStore{byUser: map[string][]recording.Recording{
		"alex": {
			{ID: "rec-1", UserID: "alex", SessionID: "s1"},
			{ID: "rec-2", UserID: "alex", SessionID: "s1"},
			{ID: "rec-3", UserID: "alex", SessionID: "s2"},
		},
	}}
	h := sessionDeleteHandler(fakeIdentifier{id: "alex", ok: true}, st, nil, recStore)

	req := httptest.NewRequest("DELETE", "/api/sessions/s1", nil)
	req.SetPathValue("id", "s1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	remaining := recStore.byUser["alex"]
	if len(remaining) != 1 || remaining[0].ID != "rec-3" {
		t.Fatalf("byUser[alex] = %+v, want only rec-3 (session s2) left", remaining)
	}
}

// TestSessionDeleteCascadesToAudioBackups verifies deleting a chat room also
// removes its temporary raw-audio backups (see transport.AudioSaver.
// DeleteBySession) — the "임시 녹음본" (temporary recordings) that otherwise
// silently outlive the conversation, since this is a separate S3-backed
// feature from the recordings archive tested above.
func TestSessionDeleteCascadesToAudioBackups(t *testing.T) {
	st := &fakeSessionStore{}
	audioStore := &fakeAudioBackupStore{byUser: map[string][]string{
		"alex": {"s1/a.pcm", "s1/b.pcm", "s2/c.pcm"},
	}}
	h := sessionDeleteHandler(fakeIdentifier{id: "alex", ok: true}, st, audioStore, nil)

	req := httptest.NewRequest("DELETE", "/api/sessions/s1", nil)
	req.SetPathValue("id", "s1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	remaining := audioStore.byUser["alex"]
	if len(remaining) != 1 || remaining[0] != "s2/c.pcm" {
		t.Fatalf("byUser[alex] = %+v, want only s2/c.pcm left", remaining)
	}
}

// TestSessionDeleteSucceedsWhenRecordingsDisabled documents that a nil
// recording.Store (archival disabled — see buildRecordingStore) doesn't
// block deleting the session itself.
func TestSessionDeleteSucceedsWhenRecordingsDisabled(t *testing.T) {
	st := &fakeSessionStore{}
	h := sessionDeleteHandler(fakeIdentifier{id: "alex", ok: true}, st, nil, nil)

	req := httptest.NewRequest("DELETE", "/api/sessions/s1", nil)
	req.SetPathValue("id", "s1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
}

// TestSessionDeleteSucceedsWhenRecordingsCascadeFails documents the
// best-effort contract: a failure cascading to recordings must not block
// deleting the session itself (same "side-effect, log only" pattern as
// transport.Handler.backupAudio).
func TestSessionDeleteSucceedsWhenRecordingsCascadeFails(t *testing.T) {
	st := &fakeSessionStore{}
	recStore := &fakeRecordingStore{err: errors.New("s3 unreachable")}
	h := sessionDeleteHandler(fakeIdentifier{id: "alex", ok: true}, st, nil, recStore)

	req := httptest.NewRequest("DELETE", "/api/sessions/s1", nil)
	req.SetPathValue("id", "s1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 even though the recordings cascade failed", rec.Code)
	}
	if len(st.deleted) != 1 {
		t.Fatalf("session delete should still have happened, deleted = %+v", st.deleted)
	}
}

// TestSessionDeleteSucceedsWhenAudioCascadeFails documents the same
// best-effort contract for the audio-backup cascade: a failure there must
// not block deleting the session itself.
func TestSessionDeleteSucceedsWhenAudioCascadeFails(t *testing.T) {
	st := &fakeSessionStore{}
	audioStore := &fakeAudioBackupStore{err: errors.New("s3 unreachable")}
	h := sessionDeleteHandler(fakeIdentifier{id: "alex", ok: true}, st, audioStore, nil)

	req := httptest.NewRequest("DELETE", "/api/sessions/s1", nil)
	req.SetPathValue("id", "s1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 even though the audio backup cascade failed", rec.Code)
	}
	if len(st.deleted) != 1 {
		t.Fatalf("session delete should still have happened, deleted = %+v", st.deleted)
	}
}
