package httpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"buddy/server/internal/llm"
	"buddy/server/internal/pipeline"
)

// fakeQuizAnswerCheckLLM is a minimal llm.Client double for exercising
// pipeline.Pipeline.CheckQuizAnswer (called from quizAnswerCheckHandler)
// without a real model — mirrors fakeWordSuggestLLM.
type fakeQuizAnswerCheckLLM struct {
	complete func(msgs []llm.Message) (string, error)
}

func (f *fakeQuizAnswerCheckLLM) ChatStream(ctx context.Context, model string, msgs []llm.Message, onToken func(string)) (string, error) {
	return "", nil
}

func (f *fakeQuizAnswerCheckLLM) Complete(ctx context.Context, model string, msgs []llm.Message, jsonMode bool) (string, error) {
	return f.complete(msgs)
}

func TestQuizAnswerCheckHandlerReturnsCorrectVerdict(t *testing.T) {
	pipe := &pipeline.Pipeline{
		LLM:       &fakeQuizAnswerCheckLLM{complete: func(msgs []llm.Message) (string, error) { return `{"correct": true}`, nil }},
		ChatModel: "m",
	}
	h := quizAnswerCheckHandler(fakeIdentifier{id: "alex", ok: true}, pipe)

	body, _ := json.Marshal(map[string]any{
		"prompt": "He ___ to school.", "answer": "goes", "acceptableAnswers": []string{"walks"}, "learnerAnswer": "commutes",
	})
	req := httptest.NewRequest("POST", "/api/quiz/check-answer", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		Correct bool `json:"correct"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !out.Correct {
		t.Fatalf("correct = false, want true")
	}
}

func TestQuizAnswerCheckHandlerReturnsFalseVerdict(t *testing.T) {
	pipe := &pipeline.Pipeline{
		LLM:       &fakeQuizAnswerCheckLLM{complete: func(msgs []llm.Message) (string, error) { return `{"correct": false}`, nil }},
		ChatModel: "m",
	}
	h := quizAnswerCheckHandler(fakeIdentifier{id: "alex", ok: true}, pipe)

	body, _ := json.Marshal(map[string]any{"prompt": "He ___ to school.", "answer": "goes", "learnerAnswer": "banana"})
	req := httptest.NewRequest("POST", "/api/quiz/check-answer", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		Correct bool `json:"correct"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Correct {
		t.Fatalf("correct = true, want false")
	}
}

func TestQuizAnswerCheckHandlerUnauthorizedWhenIdentifyFails(t *testing.T) {
	h := quizAnswerCheckHandler(fakeIdentifier{ok: false}, &pipeline.Pipeline{})

	req := httptest.NewRequest("POST", "/api/quiz/check-answer", strings.NewReader(`{"prompt":"x","answer":"y","learnerAnswer":"z"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestQuizAnswerCheckHandlerBadRequestOnMalformedJSON(t *testing.T) {
	h := quizAnswerCheckHandler(fakeIdentifier{id: "alex", ok: true}, &pipeline.Pipeline{})

	req := httptest.NewRequest("POST", "/api/quiz/check-answer", strings.NewReader(`not json`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

// TestQuizAnswerCheckHandlerRejectsMissingFields covers prompt/answer/
// learnerAnswer each being required — any one blank means there's nothing
// meaningful to grade.
func TestQuizAnswerCheckHandlerRejectsMissingFields(t *testing.T) {
	cases := []string{
		`{"answer":"y","learnerAnswer":"z"}`,
		`{"prompt":"x","learnerAnswer":"z"}`,
		`{"prompt":"x","answer":"y"}`,
		`{"prompt":"  ","answer":"y","learnerAnswer":"z"}`,
	}
	for _, body := range cases {
		h := quizAnswerCheckHandler(fakeIdentifier{id: "alex", ok: true}, &pipeline.Pipeline{})
		req := httptest.NewRequest("POST", "/api/quiz/check-answer", strings.NewReader(body))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("body=%s: status = %d, want 400", body, rec.Code)
		}
	}
}

// TestQuizAnswerCheckHandlerRejectsOverlongLearnerAnswer guards
// maxQuizAnswerCheckLen, same reasoning as TestWordSuggestHandlerRejectsOverlongQuery.
func TestQuizAnswerCheckHandlerRejectsOverlongLearnerAnswer(t *testing.T) {
	tooLong := strings.Repeat("a", maxQuizAnswerCheckLen+1)
	body, err := json.Marshal(map[string]string{"prompt": "x", "answer": "y", "learnerAnswer": tooLong})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	h := quizAnswerCheckHandler(fakeIdentifier{id: "alex", ok: true}, &pipeline.Pipeline{})

	req := httptest.NewRequest("POST", "/api/quiz/check-answer", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestQuizAnswerCheckHandlerInternalErrorOnPipelineFailure(t *testing.T) {
	pipe := &pipeline.Pipeline{
		LLM:       &fakeQuizAnswerCheckLLM{complete: func(msgs []llm.Message) (string, error) { return "", errors.New("model unreachable") }},
		ChatModel: "m",
	}
	h := quizAnswerCheckHandler(fakeIdentifier{id: "alex", ok: true}, pipe)

	body, _ := json.Marshal(map[string]string{"prompt": "x", "answer": "y", "learnerAnswer": "z"})
	req := httptest.NewRequest("POST", "/api/quiz/check-answer", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}
