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
	"buddy/server/internal/protocol"
)

// fakeWordSuggestLLM is a minimal llm.Client double for exercising
// pipeline.Pipeline.SuggestWords (called from wordSuggestHandler) without a
// real model.
type fakeWordSuggestLLM struct {
	complete func(msgs []llm.Message) (string, error)
}

func (f *fakeWordSuggestLLM) ChatStream(ctx context.Context, model string, msgs []llm.Message, onToken func(string)) (string, error) {
	return "", nil
}

func (f *fakeWordSuggestLLM) Complete(ctx context.Context, model string, msgs []llm.Message, jsonMode bool) (string, error) {
	return f.complete(msgs)
}

func TestWordSuggestHandlerReturnsSuggestions(t *testing.T) {
	pipe := &pipeline.Pipeline{
		LLM: &fakeWordSuggestLLM{complete: func(msgs []llm.Message) (string, error) {
			return `{"suggestions":[{"word":"furious","meaning":"화가 나서 참을 수 없는","example":"She was furious."}]}`, nil
		}},
		ChatModel: "m",
	}
	h := wordSuggestHandler(fakeIdentifier{id: "alex", ok: true}, pipe)

	req := httptest.NewRequest("POST", "/api/words/suggest", strings.NewReader(`{"query":"화가 나서 참을 수 없는 느낌"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Suggestions []protocol.WordSuggestion `json:"suggestions"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Suggestions) != 1 || body.Suggestions[0].Word != "furious" {
		t.Fatalf("suggestions = %+v, want one suggestion for \"furious\"", body.Suggestions)
	}
}

func TestWordSuggestHandlerUnauthorizedWhenIdentifyFails(t *testing.T) {
	h := wordSuggestHandler(fakeIdentifier{ok: false}, &pipeline.Pipeline{})

	req := httptest.NewRequest("POST", "/api/words/suggest", strings.NewReader(`{"query":"x"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestWordSuggestHandlerBadRequestOnMalformedJSON(t *testing.T) {
	h := wordSuggestHandler(fakeIdentifier{id: "alex", ok: true}, &pipeline.Pipeline{})

	req := httptest.NewRequest("POST", "/api/words/suggest", strings.NewReader(`not json`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestWordSuggestHandlerRejectsEmptyQuery(t *testing.T) {
	h := wordSuggestHandler(fakeIdentifier{id: "alex", ok: true}, &pipeline.Pipeline{})

	req := httptest.NewRequest("POST", "/api/words/suggest", strings.NewReader(`{"query":"   "}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

// TestWordSuggestHandlerRejectsOverlongQuery guards maxWordQueryLen, same
// "cap a free-text field a learner controls" reasoning as
// TestSettingsSaveRejectsOverlongStyle.
func TestWordSuggestHandlerRejectsOverlongQuery(t *testing.T) {
	tooLong := strings.Repeat("가", maxWordQueryLen+1)
	body, err := json.Marshal(map[string]string{"query": tooLong})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	h := wordSuggestHandler(fakeIdentifier{id: "alex", ok: true}, &pipeline.Pipeline{})

	req := httptest.NewRequest("POST", "/api/words/suggest", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestWordSuggestHandlerInternalErrorOnPipelineFailure(t *testing.T) {
	pipe := &pipeline.Pipeline{
		LLM: &fakeWordSuggestLLM{complete: func(msgs []llm.Message) (string, error) {
			return "", errors.New("model unreachable")
		}},
		ChatModel: "m",
	}
	h := wordSuggestHandler(fakeIdentifier{id: "alex", ok: true}, pipe)

	req := httptest.NewRequest("POST", "/api/words/suggest", strings.NewReader(`{"query":"x"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}
