package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"buddy/server/internal/recording"
)

// fakeRecordingStore is an in-memory recording.Store for handler tests —
// real S3/SQL behavior is covered by internal/recording's own
// container-backed tests.
type fakeRecordingStore struct {
	byUser map[string][]recording.Recording
	bodies map[string]string // recording ID -> stored (already "compressed") bytes
	err    error
}

func (f *fakeRecordingStore) Save(ctx context.Context, userID, sessionID string, pcm []byte, sampleRate int) (recording.Recording, error) {
	return recording.Recording{}, errors.New("not used by these tests")
}

func (f *fakeRecordingStore) List(ctx context.Context, userID string) ([]recording.Recording, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.byUser[userID], nil
}

func (f *fakeRecordingStore) Open(ctx context.Context, userID, id string) (recording.Recording, io.ReadCloser, error) {
	for _, rec := range f.byUser[userID] {
		if rec.ID == id {
			return rec, io.NopCloser(strings.NewReader(f.bodies[id])), nil
		}
	}
	return recording.Recording{}, nil, errors.New("not found")
}

// Delete removes id from userID's recordings — a no-op (not an error) if it
// isn't there, mirroring S3Store's indistinguishable-from-missing contract.
func (f *fakeRecordingStore) Delete(ctx context.Context, userID, id string) error {
	if f.err != nil {
		return f.err
	}
	out := f.byUser[userID][:0]
	for _, rec := range f.byUser[userID] {
		if rec.ID != id {
			out = append(out, rec)
		}
	}
	f.byUser[userID] = out
	return nil
}

// DeleteBySession removes every recording under sessionID — used by the
// cascading-delete handler tests.
func (f *fakeRecordingStore) DeleteBySession(ctx context.Context, userID, sessionID string) error {
	if f.err != nil {
		return f.err
	}
	out := f.byUser[userID][:0]
	for _, rec := range f.byUser[userID] {
		if rec.SessionID != sessionID {
			out = append(out, rec)
		}
	}
	f.byUser[userID] = out
	return nil
}

func (f *fakeRecordingStore) Close() error { return nil }

func TestRecordingsListReturnsOnlyCallersRecordings(t *testing.T) {
	now := time.Unix(1700000000, 0)
	store := &fakeRecordingStore{byUser: map[string][]recording.Recording{
		"alex": {{ID: "rec-1", UserID: "alex", CreatedAt: now, DurationMS: 1500, SizeBytes: 4096}},
		"sam":  {{ID: "rec-2", UserID: "sam", CreatedAt: now, DurationMS: 900, SizeBytes: 2048}},
	}}
	h := recordingsListHandler(fakeIdentifier{id: "alex", ok: true}, store)

	req := httptest.NewRequest("GET", "/api/recordings", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var body []struct {
		ID         string `json:"id"`
		DurationMS int    `json:"durationMs"`
		SizeBytes  int64  `json:"sizeBytes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("bad JSON body: %v", err)
	}
	if len(body) != 1 || body[0].ID != "rec-1" || body[0].DurationMS != 1500 || body[0].SizeBytes != 4096 {
		t.Fatalf("body = %+v, want exactly alex's rec-1", body)
	}
}

func TestRecordingsListUnauthorizedWhenIdentifyFails(t *testing.T) {
	h := recordingsListHandler(fakeIdentifier{ok: false}, &fakeRecordingStore{})

	req := httptest.NewRequest("GET", "/api/recordings", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestRecordingsListServiceUnavailableWhenDisabled(t *testing.T) {
	h := recordingsListHandler(fakeIdentifier{id: "alex", ok: true}, nil)

	req := httptest.NewRequest("GET", "/api/recordings", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

func TestRecordingAudioStreamsGzipBytesWithHeaders(t *testing.T) {
	store := &fakeRecordingStore{
		byUser: map[string][]recording.Recording{
			"alex": {{ID: "rec-1", UserID: "alex", SizeBytes: 5}},
		},
		bodies: map[string]string{"rec-1": "hello"},
	}
	h := recordingAudioHandler(fakeIdentifier{id: "alex", ok: true}, store)

	req := httptest.NewRequest("GET", "/api/recordings/rec-1/audio", nil)
	req.SetPathValue("id", "rec-1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Content-Encoding"); got != "gzip" {
		t.Errorf("Content-Encoding = %q, want gzip", got)
	}
	if got := rec.Header().Get("Content-Type"); got != "audio/wav" {
		t.Errorf("Content-Type = %q, want audio/wav", got)
	}
	if rec.Body.String() != "hello" {
		t.Errorf("body = %q, want %q", rec.Body.String(), "hello")
	}
}

// TestRecordingAudioRejectsWrongUser exercises the same IDOR-prevention
// contract as internal/recording.Store.Open's own tests: a recording ID that
// exists but doesn't belong to the caller must 404, not leak the audio.
func TestRecordingAudioRejectsWrongUser(t *testing.T) {
	store := &fakeRecordingStore{
		byUser: map[string][]recording.Recording{
			"owner": {{ID: "rec-1", UserID: "owner", SizeBytes: 5}},
		},
		bodies: map[string]string{"rec-1": "hello"},
	}
	h := recordingAudioHandler(fakeIdentifier{id: "someone-else", ok: true}, store)

	req := httptest.NewRequest("GET", "/api/recordings/rec-1/audio", nil)
	req.SetPathValue("id", "rec-1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestRecordingAudioServiceUnavailableWhenDisabled(t *testing.T) {
	h := recordingAudioHandler(fakeIdentifier{id: "alex", ok: true}, nil)

	req := httptest.NewRequest("GET", "/api/recordings/rec-1/audio", nil)
	req.SetPathValue("id", "rec-1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

func TestRecordingDeleteRemovesOnlyTheGivenRecording(t *testing.T) {
	store := &fakeRecordingStore{byUser: map[string][]recording.Recording{
		"alex": {{ID: "rec-1", UserID: "alex"}, {ID: "rec-2", UserID: "alex"}},
	}}
	h := recordingDeleteHandler(fakeIdentifier{id: "alex", ok: true}, store)

	req := httptest.NewRequest("DELETE", "/api/recordings/rec-1", nil)
	req.SetPathValue("id", "rec-1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	remaining := store.byUser["alex"]
	if len(remaining) != 1 || remaining[0].ID != "rec-2" {
		t.Fatalf("byUser[alex] = %+v, want only rec-2 left", remaining)
	}
}

func TestRecordingDeleteUnauthorizedWhenIdentifyFails(t *testing.T) {
	h := recordingDeleteHandler(fakeIdentifier{ok: false}, &fakeRecordingStore{})

	req := httptest.NewRequest("DELETE", "/api/recordings/rec-1", nil)
	req.SetPathValue("id", "rec-1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestRecordingDeleteServiceUnavailableWhenDisabled(t *testing.T) {
	h := recordingDeleteHandler(fakeIdentifier{id: "alex", ok: true}, nil)

	req := httptest.NewRequest("DELETE", "/api/recordings/rec-1", nil)
	req.SetPathValue("id", "rec-1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

func TestRecordingDeleteInternalErrorOnStoreFailure(t *testing.T) {
	store := &fakeRecordingStore{err: errors.New("s3 unreachable")}
	h := recordingDeleteHandler(fakeIdentifier{id: "alex", ok: true}, store)

	req := httptest.NewRequest("DELETE", "/api/recordings/rec-1", nil)
	req.SetPathValue("id", "rec-1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}
