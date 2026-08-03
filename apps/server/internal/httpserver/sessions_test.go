package httpserver

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"buddy/server/internal/protocol"
	"buddy/server/internal/recording"
	"buddy/server/internal/store"
)

// fakeSessionStore is an in-memory store.Store for sessionDeleteHandler
// tests — real SQL correctness (transactional two-table delete, per-user
// scoping) is covered by internal/store's own container-backed tests.
// mu guards every field sessionEndHandler's background goroutine (see
// EnqueueStudySummaryJob/RunStudySummaryInline) can write concurrently with
// a test's own polling reads — everything else here only ever runs
// synchronously within ServeHTTP.
type fakeSessionStore struct {
	mu sync.Mutex

	deleted []struct{ userID, sessionID string }
	err     error

	// detailMeta/detailTurns/detailHasMore/detailErr back SessionDetail/
	// SessionDetailPage for sessionDetailHandler tests (see
	// sessions_detail_test.go); left zero for the sessionDeleteHandler tests
	// in this file, which don't call either.
	// detailBeforeTurn/detailLimit record the pagination args
	// SessionDetailPage was actually called with; detailUnboundedCalled
	// records whether the unbounded SessionDetail was called instead — a
	// test can assert on whichever it expects the handler to have used.
	detailMeta            store.SessionMeta
	detailTurns           []store.Turn
	detailHasMore         bool
	detailErr             error
	detailBeforeTurn      int
	detailLimit           int
	detailUnboundedCalled bool

	// profile/profileErr/lastTurnVal/lastTurnErr back Load/LastTurn for
	// sessionCompactionHandler tests (see sessions_compaction_test.go); left
	// zero for tests in this file, which don't call either.
	profile     store.Profile
	profileErr  error
	lastTurnVal int
	lastTurnErr error

	// styles/styleErr back GetInterlocutorStyle/SaveInterlocutorStyle for
	// settings_test.go; left zero for tests in this file, which don't call
	// either.
	styles   map[string]string // userID -> interlocutorStyle
	styleErr error

	// endCalls/endErr back EndSession for sessions_end_test.go; left zero for
	// tests in this file, which don't call it.
	endCalls []struct{ userID, sessionID string }
	endErr   error

	// completeSummaryCalls/failSummaryCalls back CompleteStudySummary/
	// FailStudySummary for sessions_end_test.go and study_summary_job_test.go.
	completeSummaryCalls []struct {
		userID, sessionID string
		summary           []protocol.StudySummarySentence
	}
	completeSummaryErr error
	failSummaryCalls   []struct{ userID, sessionID string }
	failSummaryErr     error

	// restartSummaryCalls/restartSummaryErr back RestartStudySummary for
	// sessions_restudy_test.go; left zero for tests in this file, which
	// don't call it.
	restartSummaryCalls []struct{ userID, sessionID string }
	restartSummaryErr   error

	// completeQuizCalls/failQuizCalls back CompleteStudyQuiz/FailStudyQuiz
	// for the quiz pre-generation job's tests; left zero for tests in this
	// file, which don't call either.
	completeQuizCalls []struct {
		userID, sessionID string
		questions         []protocol.QuizQuestion
	}
	completeQuizErr error
	failQuizCalls   []struct{ userID, sessionID string }
	failQuizErr     error

	// markQuizCompletedCalls/markQuizCompletedErr back MarkQuizCompleted for
	// sessions_quiz_complete_test.go; left zero for tests in this file, which
	// don't call it.
	markQuizCompletedCalls []struct{ userID, sessionID string }
	markQuizCompletedErr   error

	// restartQuizCalls/restartQuizErr back RestartStudyQuiz for
	// sessions_quiz_reset_test.go; left zero for tests in this file, which
	// don't call it.
	restartQuizCalls []struct{ userID, sessionID string }
	restartQuizErr   error

	// learnerProfiles/learnerProfileErr back GetLearnerProfile/
	// SaveLearnerProfile for sessions_end_test.go.
	learnerProfiles   map[string]string // userID -> profile
	learnerProfileErr error

	// instantSessions/instantSessionsErr back ListInstantSessions for
	// sessions_instant_test.go; left zero for tests in this file, which
	// don't call it.
	instantSessions    []store.SessionMeta
	instantSessionsErr error

	// markInstantCalls/markInstantErr back MarkInstant for
	// sessions_instant_test.go; left zero for tests in this file, which
	// don't call it.
	markInstantCalls []struct{ userID, sessionID string }
	markInstantErr   error

	// withStudySummary/withStudySummaryErr back ListSessionsWithStudySummary
	// for sessions_delete_profile_regenerate_test.go; left zero for tests in
	// this file, which don't call it. The rebuilt result itself lands in
	// learnerProfiles above via the existing SaveLearnerProfile fake.
	// withStudySummaryCalls counts calls, so a test can assert a rebuild was
	// (or, more often, deliberately wasn't) even attempted.
	withStudySummary      []store.SessionMeta
	withStudySummaryErr   error
	withStudySummaryCalls int
}

func (f *fakeSessionStore) Load(ctx context.Context, userID, sessionID string) (store.Profile, error) {
	if f.profileErr != nil {
		return store.Profile{}, f.profileErr
	}
	return f.profile, nil
}

func (f *fakeSessionStore) Save(ctx context.Context, userID, sessionID string, p store.Profile) error {
	return errors.New("not used by these tests")
}

func (f *fakeSessionStore) SaveTurn(ctx context.Context, userID, sessionID string, turn int, role, text string, refined bool, source string) error {
	return errors.New("not used by these tests")
}

func (f *fakeSessionStore) SaveCorrection(ctx context.Context, userID, sessionID string, turn int, c protocol.Correction) error {
	return errors.New("not used by these tests")
}

func (f *fakeSessionStore) ReserveCorrectionJob(ctx context.Context, userID, sessionID string, turn int) error {
	return errors.New("not used by these tests")
}

func (f *fakeSessionStore) SaveTranslation(ctx context.Context, userID, sessionID string, turn int, role, translation string) error {
	return errors.New("not used by these tests")
}

func (f *fakeSessionStore) SaveGeneratedTitle(ctx context.Context, userID, sessionID, title string) error {
	return errors.New("not used by these tests")
}

func (f *fakeSessionStore) ReserveAssistantTurn(ctx context.Context, userID, sessionID string, turn int) error {
	return errors.New("not used by these tests")
}

func (f *fakeSessionStore) CompleteAssistantTurn(ctx context.Context, userID, sessionID string, turn int, text string) error {
	return errors.New("not used by these tests")
}

func (f *fakeSessionStore) FailJob(ctx context.Context, userID, sessionID string, turn int, kind, errMsg string) error {
	return errors.New("not used by these tests")
}

func (f *fakeSessionStore) JobStatus(ctx context.Context, userID, sessionID string, turn int, kind string) (string, error) {
	return "", errors.New("not used by these tests")
}

func (f *fakeSessionStore) AssistantTurnText(ctx context.Context, userID, sessionID string, turn int) (string, error) {
	return "", errors.New("not used by these tests")
}

func (f *fakeSessionStore) LastTurn(ctx context.Context, userID, sessionID string) (int, error) {
	if f.lastTurnErr != nil {
		return 0, f.lastTurnErr
	}
	return f.lastTurnVal, nil
}

func (f *fakeSessionStore) ListSessions(ctx context.Context, userID string) ([]store.SessionMeta, error) {
	return nil, errors.New("not used by these tests")
}

func (f *fakeSessionStore) ListInstantSessions(ctx context.Context, userID string) ([]store.SessionMeta, error) {
	if f.instantSessionsErr != nil {
		return nil, f.instantSessionsErr
	}
	return f.instantSessions, nil
}

func (f *fakeSessionStore) ListSessionsWithStudySummary(ctx context.Context, userID string) ([]store.SessionMeta, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.withStudySummaryCalls++
	if f.withStudySummaryErr != nil {
		return nil, f.withStudySummaryErr
	}
	return f.withStudySummary, nil
}

func (f *fakeSessionStore) snapshotWithStudySummaryCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.withStudySummaryCalls
}

func (f *fakeSessionStore) MarkInstant(ctx context.Context, userID, sessionID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.markInstantErr != nil {
		return f.markInstantErr
	}
	f.markInstantCalls = append(f.markInstantCalls, struct{ userID, sessionID string }{userID, sessionID})
	return nil
}

func (f *fakeSessionStore) SessionDetail(ctx context.Context, userID, sessionID string) (store.SessionMeta, []store.Turn, error) {
	f.detailUnboundedCalled = true
	if f.detailTurns == nil && f.detailErr == nil {
		return store.SessionMeta{}, nil, errors.New("not used by these tests")
	}
	if f.detailErr != nil {
		return store.SessionMeta{}, nil, f.detailErr
	}
	return f.detailMeta, f.detailTurns, nil
}

func (f *fakeSessionStore) SessionDetailPage(ctx context.Context, userID, sessionID string, beforeTurn, limit int) (store.SessionMeta, []store.Turn, bool, error) {
	f.detailBeforeTurn = beforeTurn
	f.detailLimit = limit
	if f.detailTurns == nil && f.detailErr == nil {
		return store.SessionMeta{}, nil, false, errors.New("not used by these tests")
	}
	if f.detailErr != nil {
		return store.SessionMeta{}, nil, false, f.detailErr
	}
	return f.detailMeta, f.detailTurns, f.detailHasMore, nil
}

func (f *fakeSessionStore) DeleteSession(ctx context.Context, userID, sessionID string) error {
	if f.err != nil {
		return f.err
	}
	f.deleted = append(f.deleted, struct{ userID, sessionID string }{userID, sessionID})
	return nil
}

func (f *fakeSessionStore) GetInterlocutorStyle(ctx context.Context, userID string) (string, error) {
	if f.styleErr != nil {
		return "", f.styleErr
	}
	return f.styles[userID], nil
}

func (f *fakeSessionStore) SaveInterlocutorStyle(ctx context.Context, userID, style string) error {
	if f.styleErr != nil {
		return f.styleErr
	}
	if f.styles == nil {
		f.styles = make(map[string]string)
	}
	f.styles[userID] = style
	return nil
}

func (f *fakeSessionStore) EndSession(ctx context.Context, userID, sessionID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.endErr != nil {
		return f.endErr
	}
	f.endCalls = append(f.endCalls, struct{ userID, sessionID string }{userID, sessionID})
	return nil
}

func (f *fakeSessionStore) CompleteStudySummary(ctx context.Context, userID, sessionID string, summary []protocol.StudySummarySentence) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.completeSummaryErr != nil {
		return f.completeSummaryErr
	}
	f.completeSummaryCalls = append(f.completeSummaryCalls, struct {
		userID, sessionID string
		summary           []protocol.StudySummarySentence
	}{userID, sessionID, summary})
	return nil
}

func (f *fakeSessionStore) FailStudySummary(ctx context.Context, userID, sessionID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failSummaryErr != nil {
		return f.failSummaryErr
	}
	f.failSummaryCalls = append(f.failSummaryCalls, struct{ userID, sessionID string }{userID, sessionID})
	return nil
}

func (f *fakeSessionStore) RestartStudySummary(ctx context.Context, userID, sessionID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.restartSummaryErr != nil {
		return f.restartSummaryErr
	}
	f.restartSummaryCalls = append(f.restartSummaryCalls, struct{ userID, sessionID string }{userID, sessionID})
	return nil
}

func (f *fakeSessionStore) CompleteStudyQuiz(ctx context.Context, userID, sessionID string, questions []protocol.QuizQuestion) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.completeQuizErr != nil {
		return f.completeQuizErr
	}
	f.completeQuizCalls = append(f.completeQuizCalls, struct {
		userID, sessionID string
		questions         []protocol.QuizQuestion
	}{userID, sessionID, questions})
	return nil
}

func (f *fakeSessionStore) FailStudyQuiz(ctx context.Context, userID, sessionID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failQuizErr != nil {
		return f.failQuizErr
	}
	f.failQuizCalls = append(f.failQuizCalls, struct{ userID, sessionID string }{userID, sessionID})
	return nil
}

func (f *fakeSessionStore) MarkQuizCompleted(ctx context.Context, userID, sessionID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.markQuizCompletedErr != nil {
		return f.markQuizCompletedErr
	}
	f.markQuizCompletedCalls = append(f.markQuizCompletedCalls, struct{ userID, sessionID string }{userID, sessionID})
	return nil
}

func (f *fakeSessionStore) RestartStudyQuiz(ctx context.Context, userID, sessionID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.restartQuizErr != nil {
		return f.restartQuizErr
	}
	f.restartQuizCalls = append(f.restartQuizCalls, struct{ userID, sessionID string }{userID, sessionID})
	return nil
}

func (f *fakeSessionStore) GetLearnerProfile(ctx context.Context, userID string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.learnerProfileErr != nil {
		return "", f.learnerProfileErr
	}
	return f.learnerProfiles[userID], nil
}

func (f *fakeSessionStore) SaveLearnerProfile(ctx context.Context, userID, profile string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.learnerProfileErr != nil {
		return f.learnerProfileErr
	}
	if f.learnerProfiles == nil {
		f.learnerProfiles = make(map[string]string)
	}
	f.learnerProfiles[userID] = profile
	return nil
}

func (f *fakeSessionStore) Close() error { return nil }

// snapshotEndCalls/snapshotCompleteSummaryCalls/snapshotLearnerProfile give
// tests a lock-protected read of state sessionEndHandler's background
// goroutine (see EnqueueStudySummaryJob/RunStudySummaryInline) may still be
// writing to concurrently — see waitForCondition.
func (f *fakeSessionStore) snapshotEndCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.endCalls)
}

func (f *fakeSessionStore) snapshotCompleteSummaryCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.completeSummaryCalls)
}

func (f *fakeSessionStore) snapshotFailSummaryCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.failSummaryCalls)
}

func (f *fakeSessionStore) snapshotRestartSummaryCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.restartSummaryCalls)
}

func (f *fakeSessionStore) snapshotCompleteQuizCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.completeQuizCalls)
}

func (f *fakeSessionStore) snapshotFailQuizCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.failQuizCalls)
}

func (f *fakeSessionStore) snapshotMarkQuizCompletedCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.markQuizCompletedCalls)
}

func (f *fakeSessionStore) snapshotRestartQuizCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.restartQuizCalls)
}

func (f *fakeSessionStore) snapshotLearnerProfile(userID string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.learnerProfiles[userID]
}

func (f *fakeSessionStore) snapshotMarkInstantCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.markInstantCalls)
}

// waitForCondition polls cond until it's true or timeout elapses — needed
// because sessionEndHandler's study-summary work runs on a background
// goroutine (see EnqueueStudySummaryJob/RunStudySummaryInline), so a test
// can't just check state synchronously after ServeHTTP returns.
func waitForCondition(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !cond() {
		t.Fatalf("condition not met within %s", timeout)
	}
}

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

// Delete removes one key formatted "sessionID/id" from userID's backups —
// used by recordingDeleteHandler's cascade tests (see recordings_test.go).
func (f *fakeAudioBackupStore) Delete(ctx context.Context, userID, sessionID, id string) error {
	if f.err != nil {
		return f.err
	}
	key := sessionID + "/" + id
	out := f.byUser[userID][:0]
	for _, k := range f.byUser[userID] {
		if k != key {
			out = append(out, k)
		}
	}
	f.byUser[userID] = out
	return nil
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
	h := sessionDeleteHandler(fakeIdentifier{id: "alex", ok: true}, st, nil, nil, nil, nil)

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
	h := sessionDeleteHandler(fakeIdentifier{ok: false}, &fakeSessionStore{}, nil, nil, nil, nil)

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
	h := sessionDeleteHandler(fakeIdentifier{id: "alex", ok: true}, st, nil, nil, nil, nil)

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
	h := sessionDeleteHandler(fakeIdentifier{id: "alex", ok: true}, st, nil, recStore, nil, nil)

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
	h := sessionDeleteHandler(fakeIdentifier{id: "alex", ok: true}, st, audioStore, nil, nil, nil)

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
	h := sessionDeleteHandler(fakeIdentifier{id: "alex", ok: true}, st, nil, nil, nil, nil)

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
	h := sessionDeleteHandler(fakeIdentifier{id: "alex", ok: true}, st, nil, recStore, nil, nil)

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
	h := sessionDeleteHandler(fakeIdentifier{id: "alex", ok: true}, st, audioStore, nil, nil, nil)

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
