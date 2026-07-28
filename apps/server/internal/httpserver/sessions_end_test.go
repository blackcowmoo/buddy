package httpserver

import (
	"context"
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

// fakeStudySummaryLLM is a minimal llm.Client double for exercising
// pipeline.Pipeline.GenerateStudySummary (called from the background
// study-summary job, not this handler directly) without a real model.
type fakeStudySummaryLLM struct {
	complete func(msgs []llm.Message) (string, error)
}

func (f *fakeStudySummaryLLM) ChatStream(ctx context.Context, model string, msgs []llm.Message, onToken func(string)) (string, error) {
	return "", nil
}

func (f *fakeStudySummaryLLM) Complete(ctx context.Context, model string, msgs []llm.Message, jsonMode bool) (string, error) {
	return f.complete(msgs)
}

func postEndRequest(t *testing.T) *http.Request {
	t.Helper()
	req := httptest.NewRequest("POST", "/api/sessions/s1/end", nil)
	req.SetPathValue("id", "s1")
	return req
}

// TestSessionEndHandlerFreezesImmediately guards the core fix: EndSession
// (the freeze) must complete, and the response must come back, without
// waiting on the wrap-up LLM call — a gated fake LLM that blocks until
// released proves the response isn't stuck behind it.
func TestSessionEndHandlerFreezesImmediately(t *testing.T) {
	release := make(chan struct{})
	pipe := &pipeline.Pipeline{
		Analysis: []pipeline.Candidate{{Model: "m", LLM: &fakeStudySummaryLLM{complete: func(msgs []llm.Message) (string, error) {
			<-release // never released during this test — proves ServeHTTP doesn't wait for it
			return `{"sentences":[{"english":"unused","translation":"unused"}]}`, nil
		}}}},
	}
	st := &fakeSessionStore{
		detailMeta:  store.SessionMeta{ID: "s1"},
		detailTurns: []store.Turn{{Turn: 1, Role: "user", Text: "He go to school.", Correction: &protocol.Correction{Issues: []protocol.Issue{{Type: "grammar"}}}}},
	}
	h := sessionEndHandler(fakeIdentifier{id: "alex", ok: true}, st, pipe, nil)

	done := make(chan struct{})
	go func() {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, postEndRequest(t))
		if rec.Code != http.StatusNoContent {
			t.Errorf("status = %d, want 204", rec.Code)
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("ServeHTTP blocked on the study-summary LLM call instead of returning once EndSession froze the room")
	}
	if st.snapshotEndCalls() != 1 {
		t.Fatalf("EndSession calls = %d, want 1", st.snapshotEndCalls())
	}
	close(release)
}

// TestSessionEndHandlerNoQueueEventuallyCompletesStudySummary guards the
// no-Redis fallback (studySummaryQueue == nil): the wrap-up still gets
// generated and persisted, and folded into the learner's cross-session
// profile, on a detached goroutine — see RunStudySummaryInline.
func TestSessionEndHandlerNoQueueEventuallyCompletesStudySummary(t *testing.T) {
	pipe := &pipeline.Pipeline{
		Analysis: []pipeline.Candidate{{Model: "m", LLM: &fakeStudySummaryLLM{complete: func(msgs []llm.Message) (string, error) {
			return `{"sentences":[{"english":"Focus on third-person -s.","translation":"3인칭 단수 -s에 집중하세요."}]}`, nil
		}}}},
	}
	st := &fakeSessionStore{
		detailMeta:      store.SessionMeta{ID: "s1"},
		detailTurns:     []store.Turn{{Turn: 1, Role: "user", Text: "He go to school.", Correction: &protocol.Correction{Issues: []protocol.Issue{{Type: "grammar"}}}}},
		learnerProfiles: map[string]string{"alex": "old profile"},
	}
	h := sessionEndHandler(fakeIdentifier{id: "alex", ok: true}, st, pipe, nil)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, postEndRequest(t))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}

	waitForCondition(t, 2*time.Second, func() bool { return st.snapshotCompleteSummaryCalls() == 1 })
	if got := st.snapshotLearnerProfile("alex"); got == "old profile" || got == "" {
		t.Fatalf("learner profile = %q, want it merged with the wrap-up", got)
	}
}

// TestSessionEndHandlerWithQueueEnqueuesDurableJob guards the Redis-backed
// path against the exact regression this feature fixes: the wrap-up must be
// enqueued onto asyncjob.KindStudySummary (context.Background()-scoped, not
// tied to this request), not generated inline — so it survives the learner
// navigating away right after this response.
func TestSessionEndHandlerWithQueueEnqueuesDurableJob(t *testing.T) {
	rdb := requireRedis(t)
	queue := asyncjob.NewQueue(rdb)
	pipe := &pipeline.Pipeline{
		Analysis: []pipeline.Candidate{{Model: "m", LLM: &fakeStudySummaryLLM{complete: func(msgs []llm.Message) (string, error) {
			return `{"sentences":[{"english":"Focus on third-person -s.","translation":"3인칭 단수 -s에 집중하세요."}]}`, nil
		}}}},
	}
	st := &fakeSessionStore{
		detailMeta:  store.SessionMeta{ID: "s1"},
		detailTurns: []store.Turn{{Turn: 1, Role: "user", Text: "He go to school.", Correction: &protocol.Correction{Issues: []protocol.Issue{{Type: "grammar"}}}}},
	}
	h := sessionEndHandler(fakeIdentifier{id: "alex", ok: true}, st, pipe, queue)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, postEndRequest(t))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}

	waitForCondition(t, 2*time.Second, func() bool { return st.snapshotCompleteSummaryCalls() == 1 })
}

func TestSessionEndHandlerNotFoundPropagatesStoreErrNotFound(t *testing.T) {
	st := &fakeSessionStore{endErr: store.ErrNotFound}
	h := sessionEndHandler(fakeIdentifier{id: "alex", ok: true}, st, &pipeline.Pipeline{}, nil)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, postEndRequest(t))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestSessionEndHandlerUnauthorizedWhenIdentifyFails(t *testing.T) {
	h := sessionEndHandler(fakeIdentifier{ok: false}, &fakeSessionStore{}, &pipeline.Pipeline{}, nil)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, postEndRequest(t))

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}
