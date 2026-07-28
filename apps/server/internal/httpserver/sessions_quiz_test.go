package httpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"buddy/server/internal/llm"
	"buddy/server/internal/pipeline"
	"buddy/server/internal/protocol"
	"buddy/server/internal/store"
)

func getQuizRequest(t *testing.T) *http.Request {
	t.Helper()
	req := httptest.NewRequest("GET", "/api/sessions/s1/quiz", nil)
	req.SetPathValue("id", "s1")
	return req
}

// TestSessionQuizHandlerGeneratesFromFlaggedIssues guards the primary flow:
// issues collected from the session's persisted corrections (see
// transport.CollectStudyIssues) reach GenerateStudyQuiz, and the resulting
// questions come back in the response — this is a synchronous, on-demand
// call, not a background job like the study summary.
func TestSessionQuizHandlerGeneratesFromFlaggedIssues(t *testing.T) {
	pipe := &pipeline.Pipeline{
		Analysis: []pipeline.Candidate{{Model: "m", LLM: &fakeStudySummaryLLM{complete: func(msgs []llm.Message) (string, error) {
			return `{"questions":[{"prompt":"He ___ to school.","answer":"goes","translation":"그는 학교에 가요.","explanation":"subject-verb agreement","explanationTranslation":"주어-동사 일치"}]}`, nil
		}}}},
	}
	st := &fakeSessionStore{
		detailMeta: store.SessionMeta{ID: "s1", Ended: true},
		detailTurns: []store.Turn{
			{Turn: 1, Role: "user", Text: "He go to school.", Correction: &protocol.Correction{Issues: []protocol.Issue{{Type: "grammar", Span: "go", Suggestion: "goes"}}}},
		},
	}
	h := sessionQuizHandler(fakeIdentifier{id: "alex", ok: true}, st, pipe)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, getQuizRequest(t))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got struct {
		Questions []protocol.QuizQuestion `json:"questions"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(got.Questions) != 1 || got.Questions[0].Answer != "goes" {
		t.Fatalf("questions = %+v", got.Questions)
	}
}

// TestSessionQuizHandlerSkipsLLMWhenNoIssuesFlagged mirrors
// runStudySummary's "nothing to synthesize" guard: no LLM call, just an
// empty questions array, when the session has no flagged issues to build a
// quiz from.
func TestSessionQuizHandlerSkipsLLMWhenNoIssuesFlagged(t *testing.T) {
	calls := 0
	pipe := &pipeline.Pipeline{
		Analysis: []pipeline.Candidate{{Model: "m", LLM: countingLLMHTTP{&calls}}},
	}
	st := &fakeSessionStore{
		detailMeta:  store.SessionMeta{ID: "s1", Ended: true},
		detailTurns: []store.Turn{{Turn: 1, Role: "user", Text: "I like pizza."}},
	}
	h := sessionQuizHandler(fakeIdentifier{id: "alex", ok: true}, st, pipe)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, getQuizRequest(t))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if calls != 0 {
		t.Fatalf("expected no LLM call when no issues were flagged, got %d calls", calls)
	}
	var got struct {
		Questions []protocol.QuizQuestion `json:"questions"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(got.Questions) != 0 {
		t.Fatalf("questions = %+v, want none", got.Questions)
	}
}

func TestSessionQuizHandlerNotFoundPropagatesStoreErrNotFound(t *testing.T) {
	st := &fakeSessionStore{detailErr: store.ErrNotFound}
	h := sessionQuizHandler(fakeIdentifier{id: "alex", ok: true}, st, &pipeline.Pipeline{})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, getQuizRequest(t))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestSessionQuizHandlerUnauthorizedWhenIdentifyFails(t *testing.T) {
	h := sessionQuizHandler(fakeIdentifier{ok: false}, &fakeSessionStore{}, &pipeline.Pipeline{})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, getQuizRequest(t))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

// countingLLMHTTP increments *calls on every Complete call — a local
// duplicate of transport's countingLLM double (unexported there, and this
// package's fakeStudySummaryLLM doesn't track call counts).
type countingLLMHTTP struct {
	calls *int
}

func (c countingLLMHTTP) ChatStream(ctx context.Context, model string, msgs []llm.Message, onToken func(string)) (string, error) {
	return "", nil
}

func (c countingLLMHTTP) Complete(ctx context.Context, model string, msgs []llm.Message, jsonMode bool) (string, error) {
	*c.calls++
	return "", nil
}
