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
	"time"

	"buddy/server/internal/llm"
	"buddy/server/internal/pipeline"
)

type fakeAnswerCache struct {
	result bool
	found  bool
	saves  int
	saveCh chan bool
}

func (f *fakeAnswerCache) LookupAnswer(context.Context, string, string, string, time.Time) (bool, bool, error) {
	return f.result, f.found, nil
}
func (f *fakeAnswerCache) SaveAnswer(_ context.Context, _, _, _ string, result bool, _ time.Time) error {
	f.saves++
	if f.saveCh != nil {
		f.saveCh <- result
	}
	return nil
}

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

	requireStatus(t, rec, http.StatusOK)
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

	requireStatus(t, rec, http.StatusOK)
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

func TestQuizAnswerCheckHandlerUsesCachedVerdictWithoutCallingLLM(t *testing.T) {
	cache := &fakeAnswerCache{result: true, found: true}
	pipe := &pipeline.Pipeline{LLM: &fakeQuizAnswerCheckLLM{complete: func([]llm.Message) (string, error) {
		t.Fatal("LLM called for cached answer")
		return "", nil
	}}, ChatModel: "m"}
	h := quizAnswerCheckHandler(fakeIdentifier{id: "alex", ok: true}, pipe, cache)
	req := httptest.NewRequest("POST", "/api/quiz/check-answer", strings.NewReader(`{"prompt":"He ___","answer":"goes","learnerAnswer":"walks"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	requireStatus(t, rec, http.StatusOK)
	var out struct{ Correct, Similar bool }
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !out.Correct || !out.Similar || cache.saves != 0 {
		t.Fatalf("cached response = %+v, saves = %d", out, cache.saves)
	}
}

func TestQuizAnswerCheckHandlerReturnsChatImmediatelyThenCachesRefinedVerdict(t *testing.T) {
	analysisStarted := make(chan struct{})
	releaseAnalysis := make(chan struct{})
	cache := &fakeAnswerCache{saveCh: make(chan bool, 1)}
	pipe := &pipeline.Pipeline{
		LLM: &fakeQuizAnswerCheckLLM{complete: func([]llm.Message) (string, error) {
			return `{"correct": true}`, nil
		}},
		ChatModel: "chat",
		Analysis: []pipeline.Candidate{{Model: "analysis", LLM: &fakeQuizAnswerCheckLLM{complete: func([]llm.Message) (string, error) {
			close(analysisStarted)
			<-releaseAnalysis
			return `{"correct": false}`, nil
		}}}},
		Judge: &fakeQuizAnswerCheckLLM{complete: func([]llm.Message) (string, error) {
			return `{"correct": false}`, nil
		}},
		JudgeModel: "judge",
	}
	h := quizAnswerCheckHandler(fakeIdentifier{id: "alex", ok: true}, pipe, cache)
	req := httptest.NewRequest(http.MethodPost, "/api/quiz/check-answer", strings.NewReader(
		`{"prompt":"He ___ home.","answer":"went","learnerAnswer":"goed"}`,
	))
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		h.ServeHTTP(rec, req)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("handler blocked on Analysis instead of returning the Chat verdict")
	}
	requireStatus(t, rec, http.StatusOK)
	var response struct{ Correct bool }
	if err := json.NewDecoder(rec.Body).Decode(&response); err != nil || !response.Correct {
		t.Fatalf("Chat response = %+v, err=%v; want immediate true", response, err)
	}
	select {
	case <-analysisStarted:
	case <-time.After(time.Second):
		t.Fatal("background Analysis never started")
	}
	close(releaseAnalysis)
	select {
	case refined := <-cache.saveCh:
		if refined {
			t.Fatal("cached verdict = true, want Judge's refined false verdict")
		}
	case <-time.After(time.Second):
		t.Fatal("refined Judge verdict was not cached")
	}
}

func TestQuizAnswerCheckHandlerUnauthorizedWhenIdentifyFails(t *testing.T) {
	h := quizAnswerCheckHandler(fakeIdentifier{ok: false}, &pipeline.Pipeline{})

	req := httptest.NewRequest("POST", "/api/quiz/check-answer", strings.NewReader(`{"prompt":"x","answer":"y","learnerAnswer":"z"}`))
	assertUnauthorized(t, h, req)
}

func TestQuizAnswerCheckHandlerBadRequestOnMalformedJSON(t *testing.T) {
	h := quizAnswerCheckHandler(fakeIdentifier{id: "alex", ok: true}, &pipeline.Pipeline{})

	req := httptest.NewRequest("POST", "/api/quiz/check-answer", strings.NewReader(`not json`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	requireStatus(t, rec, http.StatusBadRequest)
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

	requireStatus(t, rec, http.StatusBadRequest)
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

	requireStatus(t, rec, http.StatusInternalServerError)
}
