package httpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"buddy/server/internal/asyncjob"
	"buddy/server/internal/llm"
	"buddy/server/internal/pipeline"
	"buddy/server/internal/quizcache"
	"github.com/redis/go-redis/v9"
)

// fakeQuizVerdictCache is a quizcache.Store double: canned hit/miss plus
// call counters, so tests can pin exactly which paths touch the verdict
// cache (lookup on every check, renew only on wrong hits).
type fakeQuizVerdictCache struct {
	verdict quizcache.Verdict
	found   bool
	lookups int
	renews  int
}

func (f *fakeQuizVerdictCache) Lookup(context.Context, string) (quizcache.Verdict, bool) {
	f.lookups++
	return f.verdict, f.found
}
func (f *fakeQuizVerdictCache) Save(context.Context, string, quizcache.Verdict) error { return nil }
func (f *fakeQuizVerdictCache) Renew(context.Context, string)                         { f.renews++ }

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
	h := quizAnswerCheckHandler(fakeIdentifier{id: "alex", ok: true}, pipe, quizOwners{}, nil, nil)

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
	h := quizAnswerCheckHandler(fakeIdentifier{id: "alex", ok: true}, pipe, quizOwners{}, nil, nil)

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
	cache := &fakeQuizVerdictCache{verdict: quizcache.Verdict{Correct: true, Similar: true}, found: true}
	pipe := &pipeline.Pipeline{LLM: &fakeQuizAnswerCheckLLM{complete: func([]llm.Message) (string, error) {
		t.Fatal("LLM called for cached answer")
		return "", nil
	}}, ChatModel: "m"}
	h := quizAnswerCheckHandler(fakeIdentifier{id: "alex", ok: true}, pipe, quizOwners{}, cache, nil)
	req := httptest.NewRequest("POST", "/api/quiz/check-answer", strings.NewReader(`{"prompt":"He ___","answer":"goes","learnerAnswer":"walks"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	requireStatus(t, rec, http.StatusOK)
	var out struct{ Correct, Similar bool }
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !out.Correct || !out.Similar || cache.lookups != 1 {
		t.Fatalf("cached response = %+v, lookups = %d", out, cache.lookups)
	}
	// A verdict the learner keeps getting RIGHT needs no renewal: it is
	// already serving its purpose and expires quietly if abandoned.
	if cache.renews != 0 {
		t.Fatalf("renews = %d for a correct hit, want 0", cache.renews)
	}
}

func TestQuizAnswerCheckHandlerRenewsCachedWrongVerdict(t *testing.T) {
	cache := &fakeQuizVerdictCache{verdict: quizcache.Verdict{Correct: false, Similar: false}, found: true}
	pipe := &pipeline.Pipeline{LLM: &fakeQuizAnswerCheckLLM{complete: func([]llm.Message) (string, error) {
		t.Fatal("LLM called for cached answer")
		return "", nil
	}}, ChatModel: "m"}
	h := quizAnswerCheckHandler(fakeIdentifier{id: "alex", ok: true}, pipe, quizOwners{}, cache, nil)
	req := httptest.NewRequest("POST", "/api/quiz/check-answer", strings.NewReader(`{"prompt":"He ___ home.","answer":"went","learnerAnswer":"goed"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	requireStatus(t, rec, http.StatusOK)
	var out struct{ Correct, Similar bool }
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Correct || out.Similar {
		t.Fatalf("cached wrong verdict returned as correct: %+v", out)
	}
	if cache.renews != 1 {
		t.Fatalf("renews = %d for a wrong hit, want 1", cache.renews)
	}
}

func TestQuizAnswerCheckHandlerReturnsChatVerdictDespiteDegradedRefineQueue(t *testing.T) {
	// A Redis outage mid-enqueue must not fail the check: the learner already
	// has Chat's verdict, and the refine can simply be recomputed on the next
	// equivalent answer.
	pipe := &pipeline.Pipeline{
		LLM:       &fakeQuizAnswerCheckLLM{complete: func([]llm.Message) (string, error) { return `{"correct":true}`, nil }},
		ChatModel: "chat",
	}
	deadRedis := redis.NewClient(&redis.Options{
		Addr:        "127.0.0.1:1",
		MaxRetries:  -1,
		DialTimeout: 100 * time.Millisecond,
		Dialer: func(context.Context, string, string) (net.Conn, error) {
			return nil, errors.New("dial blocked by test")
		},
	})
	t.Cleanup(func() { _ = deadRedis.Close() })
	h := quizAnswerCheckHandler(fakeIdentifier{id: "alex", ok: true}, pipe, quizOwners{}, quizcache.New(deadRedis, time.Minute), asyncjob.NewQueue(deadRedis))
	req := httptest.NewRequest(http.MethodPost, "/api/quiz/check-answer", strings.NewReader(
		`{"prompt":"He ___ home.","answer":"went","learnerAnswer":"goed"}`,
	))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	requireStatus(t, rec, http.StatusOK)
	var response struct{ Correct bool }
	if err := json.NewDecoder(rec.Body).Decode(&response); err != nil || !response.Correct {
		t.Fatalf("response = %+v, err = %v; want Chat's true verdict despite the dead queue", response, err)
	}
}

func TestQuizAnswerCheckHandlerUnauthorizedWhenIdentifyFails(t *testing.T) {
	h := quizAnswerCheckHandler(fakeIdentifier{ok: false}, &pipeline.Pipeline{}, quizOwners{}, nil, nil)

	req := httptest.NewRequest("POST", "/api/quiz/check-answer", strings.NewReader(`{"prompt":"x","answer":"y","learnerAnswer":"z"}`))
	assertUnauthorized(t, h, req)
}

func TestQuizAnswerCheckHandlerBadRequestOnMalformedJSON(t *testing.T) {
	h := quizAnswerCheckHandler(fakeIdentifier{id: "alex", ok: true}, &pipeline.Pipeline{}, quizOwners{}, nil, nil)

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
		h := quizAnswerCheckHandler(fakeIdentifier{id: "alex", ok: true}, &pipeline.Pipeline{}, quizOwners{}, nil, nil)
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
	h := quizAnswerCheckHandler(fakeIdentifier{id: "alex", ok: true}, &pipeline.Pipeline{}, quizOwners{}, nil, nil)

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
	h := quizAnswerCheckHandler(fakeIdentifier{id: "alex", ok: true}, pipe, quizOwners{}, nil, nil)

	body, _ := json.Marshal(map[string]string{"prompt": "x", "answer": "y", "learnerAnswer": "z"})
	req := httptest.NewRequest("POST", "/api/quiz/check-answer", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	requireStatus(t, rec, http.StatusInternalServerError)
}
