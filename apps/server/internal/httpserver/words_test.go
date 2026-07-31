package httpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"buddy/server/internal/llm"
	"buddy/server/internal/pipeline"
	"buddy/server/internal/protocol"
	"buddy/server/internal/wordreview"
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

// fakeWordStore is an in-memory wordreview.Store for handler tests — real
// SQL behavior (upsert-on-duplicate, schedule math) is covered by
// internal/wordreview's own tests. Guarded by mu because wordSaveHandler
// fires a background goroutine (see enqueueOrRunInline) that touches the
// store concurrently with the test's own post-request assertions.
type fakeWordStore struct {
	mu     sync.Mutex
	byUser map[string][]wordreview.Word
	err    error
}

func (f *fakeWordStore) Save(ctx context.Context, userID, word, meaning, example string) (wordreview.Word, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return wordreview.Word{}, f.err
	}
	for _, w := range f.byUser[userID] {
		if w.Word == word && w.Meaning == meaning {
			return w, nil
		}
	}
	saved := wordreview.Word{ID: "new-id", UserID: userID, Word: word, Meaning: meaning, Example: example, Status: wordreview.StatusPending}
	if f.byUser == nil {
		f.byUser = map[string][]wordreview.Word{}
	}
	f.byUser[userID] = append(f.byUser[userID], saved)
	return saved, nil
}

func (f *fakeWordStore) Get(ctx context.Context, userID, id string) (wordreview.Word, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return wordreview.Word{}, f.err
	}
	for _, w := range f.byUser[userID] {
		if w.ID == id {
			return w, nil
		}
	}
	return wordreview.Word{}, nil
}

func (f *fakeWordStore) List(ctx context.Context, userID string) ([]wordreview.Word, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	return f.byUser[userID], nil
}

func (f *fakeWordStore) DueCount(ctx context.Context, userID string, now time.Time) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return 0, f.err
	}
	n := 0
	for _, w := range f.byUser[userID] {
		if w.Status == wordreview.StatusVerified && !w.NextReviewAt.After(now) {
			n++
		}
	}
	return n, nil
}

func (f *fakeWordStore) MarkVerified(ctx context.Context, userID, id string, now time.Time) (wordreview.Word, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return wordreview.Word{}, f.err
	}
	for i, w := range f.byUser[userID] {
		if w.ID == id {
			w.Status = wordreview.StatusVerified
			f.byUser[userID][i] = w
			return w, nil
		}
	}
	return wordreview.Word{}, nil
}

func (f *fakeWordStore) MarkRejected(ctx context.Context, userID, id string, reason string) (wordreview.Word, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return wordreview.Word{}, f.err
	}
	for i, w := range f.byUser[userID] {
		if w.ID == id {
			w.Status = wordreview.StatusRejected
			w.VerifyReason = reason
			f.byUser[userID][i] = w
			return w, nil
		}
	}
	return wordreview.Word{}, nil
}

func (f *fakeWordStore) Review(ctx context.Context, userID, id string, correct bool, now time.Time) (wordreview.Word, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return wordreview.Word{}, f.err
	}
	for i, w := range f.byUser[userID] {
		if w.ID == id {
			w.ReviewCount++
			f.byUser[userID][i] = w
			return w, nil
		}
	}
	return wordreview.Word{}, nil
}

func (f *fakeWordStore) Delete(ctx context.Context, userID, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	out := f.byUser[userID][:0]
	for _, w := range f.byUser[userID] {
		if w.ID != id {
			out = append(out, w)
		}
	}
	f.byUser[userID] = out
	return nil
}

func (f *fakeWordStore) Close() error { return nil }

// noopVerifyPipeline is passed to wordSaveHandler in tests that don't care
// about verification's outcome — VerifyWord errors immediately (no Analysis
// candidates configured), which enqueueOrRunInline's inline path just logs,
// same as a real deployment with a briefly-unreachable model would.
func noopVerifyPipeline() *pipeline.Pipeline { return &pipeline.Pipeline{} }

func TestWordSaveStoresLearnerChosenSuggestionAsPending(t *testing.T) {
	store := &fakeWordStore{}
	h := wordSaveHandler(fakeIdentifier{id: "alex", ok: true}, store, noopVerifyPipeline(), nil)

	body, _ := json.Marshal(map[string]string{"word": "ecstatic", "meaning": "매우 행복한", "example": "She was ecstatic."})
	req := httptest.NewRequest("POST", "/api/words/save", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var out wordItem
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("bad JSON body: %v", err)
	}
	if out.Status != wordreview.StatusPending {
		t.Errorf("status = %q, want %q — verification runs in the background, never blocks this response", out.Status, wordreview.StatusPending)
	}

	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.byUser["alex"]) != 1 || store.byUser["alex"][0].Word != "ecstatic" {
		t.Fatalf("byUser[alex] = %+v, want one saved word", store.byUser["alex"])
	}
}

func TestWordSaveTracksSameWordDifferentMeaningsIndependently(t *testing.T) {
	store := &fakeWordStore{}
	h := wordSaveHandler(fakeIdentifier{id: "alex", ok: true}, store, noopVerifyPipeline(), nil)

	riverbank, _ := json.Marshal(map[string]string{"word": "bank", "meaning": "강둑", "example": "They sat on the bank."})
	req1 := httptest.NewRequest("POST", "/api/words/save", bytes.NewReader(riverbank))
	rec1 := httptest.NewRecorder()
	h.ServeHTTP(rec1, req1)

	financial, _ := json.Marshal(map[string]string{"word": "bank", "meaning": "은행", "example": "I went to the bank."})
	req2 := httptest.NewRequest("POST", "/api/words/save", bytes.NewReader(financial))
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req2)

	store.mu.Lock()
	defer store.mu.Unlock()
	words := store.byUser["alex"]
	if len(words) != 2 {
		t.Fatalf("byUser[alex] = %+v, want two independent rows for the same word, different meanings", words)
	}
}

func TestWordSaveRejectsEmptyWord(t *testing.T) {
	h := wordSaveHandler(fakeIdentifier{id: "alex", ok: true}, &fakeWordStore{}, noopVerifyPipeline(), nil)

	body, _ := json.Marshal(map[string]string{"word": "  ", "meaning": "x", "example": "y"})
	req := httptest.NewRequest("POST", "/api/words/save", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestWordSaveUnauthorizedWhenIdentifyFails(t *testing.T) {
	h := wordSaveHandler(fakeIdentifier{ok: false}, &fakeWordStore{}, noopVerifyPipeline(), nil)

	body, _ := json.Marshal(map[string]string{"word": "ecstatic"})
	req := httptest.NewRequest("POST", "/api/words/save", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestWordsListReturnsOwnWordsAndDueCount(t *testing.T) {
	// wordsListHandler calls DueCount with the real time.Now(), so due/not-due
	// fixtures must be relative to now, not a frozen historical timestamp.
	now := time.Now()
	store := &fakeWordStore{byUser: map[string][]wordreview.Word{
		"alex": {
			{ID: "w1", UserID: "alex", Word: "ecstatic", Status: wordreview.StatusVerified, NextReviewAt: now.Add(-time.Hour)},    // due
			{ID: "w2", UserID: "alex", Word: "elated", Status: wordreview.StatusVerified, NextReviewAt: now.Add(48 * time.Hour)}, // not due yet
		},
		"sam": {{ID: "w4", UserID: "sam", Word: "other", Status: wordreview.StatusVerified, NextReviewAt: now.Add(-time.Hour)}},
	}}
	h := wordsListHandler(fakeIdentifier{id: "alex", ok: true}, store)

	req := httptest.NewRequest("GET", "/api/words", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var body struct {
		Words []struct {
			ID string `json:"id"`
		} `json:"words"`
		DueCount int `json:"dueCount"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("bad JSON body: %v", err)
	}
	if len(body.Words) != 2 {
		t.Fatalf("words len = %d, want 2 (alex's own, sam's excluded)", len(body.Words))
	}
	if body.DueCount != 1 {
		t.Fatalf("dueCount = %d, want 1 (only w1 due)", body.DueCount)
	}
}

// TestWordsListDueCountExcludesPendingAndRejected guards the verification
// gate directly: a word must never count toward (or appear in) a review
// session until the model-consensus check has actually passed it, even if
// its nextReviewAt happens to already be in the past.
func TestWordsListDueCountExcludesPendingAndRejected(t *testing.T) {
	now := time.Now()
	store := &fakeWordStore{byUser: map[string][]wordreview.Word{
		"alex": {
			{ID: "w1", UserID: "alex", Word: "ecstatic", Status: wordreview.StatusPending, NextReviewAt: now.Add(-time.Hour)},
			{ID: "w2", UserID: "alex", Word: "xyzzy", Status: wordreview.StatusRejected, NextReviewAt: now.Add(-time.Hour), VerifyReason: "not a real word"},
		},
	}}
	h := wordsListHandler(fakeIdentifier{id: "alex", ok: true}, store)

	req := httptest.NewRequest("GET", "/api/words", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	var body struct {
		DueCount int `json:"dueCount"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("bad JSON body: %v", err)
	}
	if body.DueCount != 0 {
		t.Fatalf("dueCount = %d, want 0 (pending/rejected words are never due)", body.DueCount)
	}
}

func TestWordsListUnauthorizedWhenIdentifyFails(t *testing.T) {
	h := wordsListHandler(fakeIdentifier{ok: false}, &fakeWordStore{})

	req := httptest.NewRequest("GET", "/api/words", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestWordReviewUpdatesAndReturnsWord(t *testing.T) {
	store := &fakeWordStore{byUser: map[string][]wordreview.Word{
		"alex": {{ID: "w1", UserID: "alex", Word: "ecstatic"}},
	}}
	h := wordReviewHandler(fakeIdentifier{id: "alex", ok: true}, store)

	body, _ := json.Marshal(map[string]bool{"correct": true})
	req := httptest.NewRequest("POST", "/api/words/w1/review", bytes.NewReader(body))
	req.SetPathValue("id", "w1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		ReviewCount int `json:"reviewCount"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("bad JSON body: %v", err)
	}
	if out.ReviewCount != 1 {
		t.Fatalf("reviewCount = %d, want 1", out.ReviewCount)
	}
}

func TestWordReviewNotFoundForUnknownID(t *testing.T) {
	h := wordReviewHandler(fakeIdentifier{id: "alex", ok: true}, &fakeWordStore{})

	body, _ := json.Marshal(map[string]bool{"correct": true})
	req := httptest.NewRequest("POST", "/api/words/missing/review", bytes.NewReader(body))
	req.SetPathValue("id", "missing")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestWordDeleteRemovesOnlyTheGivenWord(t *testing.T) {
	store := &fakeWordStore{byUser: map[string][]wordreview.Word{
		"alex": {{ID: "w1", UserID: "alex"}, {ID: "w2", UserID: "alex"}},
	}}
	h := wordDeleteHandler(fakeIdentifier{id: "alex", ok: true}, store)

	req := httptest.NewRequest("DELETE", "/api/words/w1", nil)
	req.SetPathValue("id", "w1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	remaining := store.byUser["alex"]
	if len(remaining) != 1 || remaining[0].ID != "w2" {
		t.Fatalf("byUser[alex] = %+v, want only w2 left", remaining)
	}
}

func TestWordDeleteInternalErrorOnStoreFailure(t *testing.T) {
	store := &fakeWordStore{err: errors.New("db unreachable")}
	h := wordDeleteHandler(fakeIdentifier{id: "alex", ok: true}, store)

	req := httptest.NewRequest("DELETE", "/api/words/w1", nil)
	req.SetPathValue("id", "w1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}
