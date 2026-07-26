package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"buddy/server/internal/llm"
	"buddy/server/internal/pipeline"
	"buddy/server/internal/protocol"
	"buddy/server/internal/store"
)

// fakeStudySummaryLLM is a minimal llm.Client double for exercising
// sessionStudySummaryHandler's pipeline.Pipeline.GenerateStudySummary call
// without a real model — same shape as pipeline_test.go's fakeLLM, kept
// local since httpserver has no existing llm.Client double.
type fakeStudySummaryLLM struct {
	complete func(msgs []llm.Message) (string, error)
}

func (f *fakeStudySummaryLLM) ChatStream(ctx context.Context, model string, msgs []llm.Message, onToken func(string)) (string, error) {
	return "", nil
}

func (f *fakeStudySummaryLLM) Complete(ctx context.Context, model string, msgs []llm.Message, jsonMode bool) (string, error) {
	return f.complete(msgs)
}

func TestSessionStudySummarySkipsLLMWhenNoIssuesFlagged(t *testing.T) {
	calls := 0
	pipe := &pipeline.Pipeline{
		Analysis: []pipeline.Candidate{{Model: "m", LLM: &fakeStudySummaryLLM{complete: func(msgs []llm.Message) (string, error) {
			calls++
			return "should not be called", nil
		}}}},
	}
	st := &fakeSessionStore{
		detailMeta: store.SessionMeta{ID: "s1"},
		detailTurns: []store.Turn{
			{Turn: 1, Role: "user", Text: "I like pizza.", Correction: &protocol.Correction{Original: "I like pizza.", Corrected: "I like pizza."}}, // no issues
			{Turn: 1, Role: "assistant", Text: "Nice!"},
		},
	}
	h := sessionStudySummaryHandler(fakeIdentifier{id: "alex", ok: true}, st, pipe)

	req := httptest.NewRequest("GET", "/api/sessions/s1/study-summary", nil)
	req.SetPathValue("id", "s1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if calls != 0 {
		t.Fatalf("expected no LLM call when no issues were flagged, got %d calls", calls)
	}
	var body struct {
		Summary    string `json:"summary"`
		IssueCount int    `json:"issueCount"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body.Summary != "" || body.IssueCount != 0 {
		t.Fatalf("response = %+v, want empty summary and zero issueCount", body)
	}
}

// TestSessionStudySummaryCollectsIssuesAcrossTurns guards the aggregation
// itself: every issue from every user turn's correction (not just the last
// one, and never from an assistant turn) must reach GenerateStudySummary.
func TestSessionStudySummaryCollectsIssuesAcrossTurns(t *testing.T) {
	pipe := &pipeline.Pipeline{
		Analysis: []pipeline.Candidate{{Model: "m", LLM: &fakeStudySummaryLLM{complete: func(msgs []llm.Message) (string, error) {
			return "focus on subject-verb agreement", nil
		}}}},
	}
	st := &fakeSessionStore{
		detailMeta: store.SessionMeta{ID: "s1"},
		detailTurns: []store.Turn{
			{Turn: 1, Role: "user", Text: "He go to school.", Correction: &protocol.Correction{
				Issues: []protocol.Issue{{Type: "grammar", Span: "go", Suggestion: "goes"}},
			}},
			{Turn: 1, Role: "assistant", Text: "Nice!", Correction: &protocol.Correction{
				Issues: []protocol.Issue{{Type: "grammar", Span: "should not count"}},
			}},
			{Turn: 2, Role: "user", Text: "I has a dog.", Correction: &protocol.Correction{
				Issues: []protocol.Issue{{Type: "grammar", Span: "has", Suggestion: "have"}},
			}},
		},
	}
	h := sessionStudySummaryHandler(fakeIdentifier{id: "alex", ok: true}, st, pipe)

	req := httptest.NewRequest("GET", "/api/sessions/s1/study-summary", nil)
	req.SetPathValue("id", "s1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var body struct {
		Summary    string `json:"summary"`
		IssueCount int    `json:"issueCount"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body.IssueCount != 2 {
		t.Fatalf("issueCount = %d, want 2 (only the two user-turn issues)", body.IssueCount)
	}
	if body.Summary != "focus on subject-verb agreement" {
		t.Fatalf("summary = %q", body.Summary)
	}
}

func TestSessionStudySummaryPropagatesGenerateError(t *testing.T) {
	pipe := &pipeline.Pipeline{
		Analysis: []pipeline.Candidate{{Model: "m", LLM: &fakeStudySummaryLLM{complete: func(msgs []llm.Message) (string, error) {
			return "", errors.New("down")
		}}}},
	}
	st := &fakeSessionStore{
		detailMeta: store.SessionMeta{ID: "s1"},
		detailTurns: []store.Turn{
			{Turn: 1, Role: "user", Text: "He go to school.", Correction: &protocol.Correction{
				Issues: []protocol.Issue{{Type: "grammar", Span: "go", Suggestion: "goes"}},
			}},
		},
	}
	h := sessionStudySummaryHandler(fakeIdentifier{id: "alex", ok: true}, st, pipe)

	req := httptest.NewRequest("GET", "/api/sessions/s1/study-summary", nil)
	req.SetPathValue("id", "s1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

func TestSessionStudySummaryUnauthorizedWhenIdentifyFails(t *testing.T) {
	h := sessionStudySummaryHandler(fakeIdentifier{ok: false}, &fakeSessionStore{}, &pipeline.Pipeline{})

	req := httptest.NewRequest("GET", "/api/sessions/s1/study-summary", nil)
	req.SetPathValue("id", "s1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestSessionStudySummaryNotFoundPropagatesStoreErrNotFound(t *testing.T) {
	st := &fakeSessionStore{detailErr: store.ErrNotFound}
	h := sessionStudySummaryHandler(fakeIdentifier{id: "alex", ok: true}, st, &pipeline.Pipeline{})

	req := httptest.NewRequest("GET", "/api/sessions/s1/study-summary", nil)
	req.SetPathValue("id", "s1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}
