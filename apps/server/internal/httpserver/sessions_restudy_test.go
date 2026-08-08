package httpserver

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"buddy/server/internal/asyncjob"
	"buddy/server/internal/llm"
	"buddy/server/internal/pipeline"
	"buddy/server/internal/protocol"
	"buddy/server/internal/store"
)

func postRestudyRequest(t *testing.T) *http.Request {
	t.Helper()
	req := httptest.NewRequest("POST", "/api/sessions/s1/restudy", nil)
	req.SetPathValue("id", "s1")
	return req
}

// doneEmptySummaryFixture builds the exact meta/turns shape
// sessionRestudyHandler is meant to recover: an ended session whose wrap-up
// landed as JobStatusDone with nothing in it, despite a real flagged issue
// still sitting in the transcript.
func doneEmptySummaryFixture() ([]store.Turn, store.SessionMeta) {
	turns := []store.Turn{{Turn: 1, Role: "user", Text: "He go to school.", Correction: &protocol.Correction{Issues: []protocol.Issue{{Type: "grammar"}}}}}
	meta := store.SessionMeta{ID: "s1", Ended: true, StudySummaryStatus: store.JobStatusDone}
	return turns, meta
}

// TestSessionRestudyHandlerNoQueueEventuallyCompletesStudySummary guards the
// core recovery path (no-Redis fallback): a session stuck showing "no
// issues" must have RestartStudySummary called (flipping it back to
// pending) and, from there, the exact same background regeneration
// sessionEndHandler already uses — landing as JobStatusDone with real
// content this time.
func TestSessionRestudyHandlerNoQueueEventuallyCompletesStudySummary(t *testing.T) {
	turns, meta := doneEmptySummaryFixture()
	pipe := &pipeline.Pipeline{
		Analysis: []pipeline.Candidate{{Model: "m", LLM: &fakeStudySummaryLLM{complete: func(msgs []llm.Message) (string, error) {
			return `{"sentences":[{"english":"Focus on third-person -s.","translation":"3인칭 단수 -s에 집중하세요."}]}`, nil
		}}}},
	}
	st := &fakeSessionStore{detailMeta: meta, detailTurns: turns}
	h := sessionRestudyHandler(fakeIdentifier{id: "alex", ok: true}, st, pipe, nil)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, postRestudyRequest(t))
	requireStatus(t, rec, http.StatusNoContent)
	if st.snapshotRestartSummaryCalls() != 1 {
		t.Fatalf("RestartStudySummary calls = %d, want 1", st.snapshotRestartSummaryCalls())
	}

	waitForCondition(t, 2*time.Second, func() bool { return st.snapshotCompleteSummaryCalls() == 1 })
}

// TestSessionRestudyHandlerWithQueueEnqueuesDurableJob mirrors
// TestSessionEndHandlerWithQueueEnqueuesDurableJob: the regeneration must go
// through asyncjob.KindStudySummary (context.Background()-scoped), not run
// inline tied to this request.
func TestSessionRestudyHandlerWithQueueEnqueuesDurableJob(t *testing.T) {
	rdb := requireRedis(t)
	queue := asyncjob.NewQueue(rdb)
	turns, meta := doneEmptySummaryFixture()
	pipe := &pipeline.Pipeline{
		Analysis: []pipeline.Candidate{{Model: "m", LLM: &fakeStudySummaryLLM{complete: func(msgs []llm.Message) (string, error) {
			return `{"sentences":[{"english":"Focus on third-person -s.","translation":"3인칭 단수 -s에 집중하세요."}]}`, nil
		}}}},
	}
	st := &fakeSessionStore{detailMeta: meta, detailTurns: turns}
	h := sessionRestudyHandler(fakeIdentifier{id: "alex", ok: true}, st, pipe, queue)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, postRestudyRequest(t))
	requireStatus(t, rec, http.StatusNoContent)

	waitForCondition(t, 2*time.Second, func() bool { return st.snapshotCompleteSummaryCalls() == 1 })
}

// TestSessionRestudyHandlerRejectsWhenSummaryAlreadyHasContent guards
// against clobbering a wrap-up that already has real feedback in it — the
// button (and this handler) only ever apply to the "done but empty" state,
// never to overwrite something a learner can already see.
func TestSessionRestudyHandlerRejectsWhenSummaryAlreadyHasContent(t *testing.T) {
	turns, meta := doneEmptySummaryFixture()
	meta.StudySummary = []protocol.StudySummarySentence{{English: "Already have feedback.", Translation: "이미 피드백이 있어요."}}
	st := &fakeSessionStore{detailMeta: meta, detailTurns: turns}
	h := sessionRestudyHandler(fakeIdentifier{id: "alex", ok: true}, st, &pipeline.Pipeline{}, nil)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, postRestudyRequest(t))

	requireStatus(t, rec, http.StatusConflict)
	if st.snapshotRestartSummaryCalls() != 0 {
		t.Fatalf("RestartStudySummary calls = %d, want 0", st.snapshotRestartSummaryCalls())
	}
}

// TestSessionRestudyHandlerAcceptsLegacyEmptyStatus guards a session ended
// before the wrap-up became an async job, which reads StudySummaryStatus as
// "" rather than JobStatusDone (see the store.SessionMeta.StudySummaryStatus
// doc comment). EndConversationControl treats "" the same as done and shows
// the button for it, so the handler must accept it too instead of 409ing
// every legacy session the instant a learner taps "다시 확인하기".
func TestSessionRestudyHandlerAcceptsLegacyEmptyStatus(t *testing.T) {
	turns, meta := doneEmptySummaryFixture()
	meta.StudySummaryStatus = ""
	pipe := &pipeline.Pipeline{
		Analysis: []pipeline.Candidate{{Model: "m", LLM: &fakeStudySummaryLLM{complete: func(msgs []llm.Message) (string, error) {
			return `{"sentences":[{"english":"Focus on third-person -s.","translation":"3인칭 단수 -s에 집중하세요."}]}`, nil
		}}}},
	}
	st := &fakeSessionStore{detailMeta: meta, detailTurns: turns}
	h := sessionRestudyHandler(fakeIdentifier{id: "alex", ok: true}, st, pipe, nil)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, postRestudyRequest(t))
	requireStatus(t, rec, http.StatusNoContent)
	if st.snapshotRestartSummaryCalls() != 1 {
		t.Fatalf("RestartStudySummary calls = %d, want 1", st.snapshotRestartSummaryCalls())
	}

	waitForCondition(t, 2*time.Second, func() bool { return st.snapshotCompleteSummaryCalls() == 1 })
}

// TestSessionRestudyHandlerRejectsWhenStillPending guards against
// double-triggering a regeneration that's already in flight.
func TestSessionRestudyHandlerRejectsWhenStillPending(t *testing.T) {
	turns, meta := doneEmptySummaryFixture()
	meta.StudySummaryStatus = store.JobStatusPending
	st := &fakeSessionStore{detailMeta: meta, detailTurns: turns}
	h := sessionRestudyHandler(fakeIdentifier{id: "alex", ok: true}, st, &pipeline.Pipeline{}, nil)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, postRestudyRequest(t))

	requireStatus(t, rec, http.StatusConflict)
}

// TestSessionRestudyHandlerRejectsWhenNotEnded guards against forcing a
// wrap-up on a room that hasn't even been ended yet.
func TestSessionRestudyHandlerRejectsWhenNotEnded(t *testing.T) {
	turns, meta := doneEmptySummaryFixture()
	meta.Ended = false
	st := &fakeSessionStore{detailMeta: meta, detailTurns: turns}
	h := sessionRestudyHandler(fakeIdentifier{id: "alex", ok: true}, st, &pipeline.Pipeline{}, nil)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, postRestudyRequest(t))

	requireStatus(t, rec, http.StatusConflict)
}

func TestSessionRestudyHandlerNotFoundPropagatesStoreErrNotFound(t *testing.T) {
	st := &fakeSessionStore{detailErr: store.ErrNotFound}
	h := sessionRestudyHandler(fakeIdentifier{id: "alex", ok: true}, st, &pipeline.Pipeline{}, nil)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, postRestudyRequest(t))

	requireStatus(t, rec, http.StatusNotFound)
}

func TestSessionRestudyHandlerUnauthorizedWhenIdentifyFails(t *testing.T) {
	h := sessionRestudyHandler(fakeIdentifier{ok: false}, &fakeSessionStore{}, &pipeline.Pipeline{}, nil)

	assertUnauthorized(t, h, postRestudyRequest(t))
}
