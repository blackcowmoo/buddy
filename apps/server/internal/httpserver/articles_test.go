package httpserver

import (
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
	"buddy/server/internal/newsarticle"
	"buddy/server/internal/newsfeed"
	"buddy/server/internal/pipeline"
)

// fakeArticleStore is an in-memory newsarticle.Store for handler tests —
// real SQL behavior (the URL-keyed cache upsert, the instance/article join)
// is covered by internal/newsarticle's own tests.
type fakeArticleStore struct {
	mu         sync.Mutex
	byURL      map[string]newsarticle.Article
	byUser     map[string][]newsarticle.Instance
	err        error
	usedURLErr error
}

func (f *fakeArticleStore) FindArticleByURL(ctx context.Context, url string) (newsarticle.Article, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return newsarticle.Article{}, false, f.err
	}
	a, ok := f.byURL[url]
	return a, ok, nil
}

func (f *fakeArticleStore) SaveArticle(ctx context.Context, a newsarticle.Article) (newsarticle.Article, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return newsarticle.Article{}, f.err
	}
	if existing, ok := f.byURL[a.URL]; ok {
		return existing, nil
	}
	a.ID = "article-" + a.URL
	if f.byURL == nil {
		f.byURL = map[string]newsarticle.Article{}
	}
	f.byURL[a.URL] = a
	return a, nil
}

func (f *fakeArticleStore) UsedURLs(ctx context.Context, userID string) (map[string]bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.usedURLErr != nil {
		return nil, f.usedURLErr
	}
	out := map[string]bool{}
	for _, inst := range f.byUser[userID] {
		out[inst.Article.URL] = true
	}
	return out, nil
}

func (f *fakeArticleStore) CreateInstance(ctx context.Context, userID, articleID string) (newsarticle.Instance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return newsarticle.Instance{}, f.err
	}
	var article newsarticle.Article
	for _, a := range f.byURL {
		if a.ID == articleID {
			article = a
			break
		}
	}
	inst := newsarticle.Instance{
		ID: "instance-" + articleID, UserID: userID, Article: article,
		SelectedIndex: -1, CreatedAt: time.Now(),
	}
	if f.byUser == nil {
		f.byUser = map[string][]newsarticle.Instance{}
	}
	f.byUser[userID] = append(f.byUser[userID], inst)
	return inst, nil
}

func (f *fakeArticleStore) List(ctx context.Context, userID string) ([]newsarticle.Instance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	return f.byUser[userID], nil
}

func (f *fakeArticleStore) Get(ctx context.Context, userID, id string) (newsarticle.Instance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return newsarticle.Instance{}, f.err
	}
	for _, inst := range f.byUser[userID] {
		if inst.ID == id {
			return inst, nil
		}
	}
	return newsarticle.Instance{}, nil
}

func (f *fakeArticleStore) Answer(ctx context.Context, userID, id string, selectedIndex int) (newsarticle.Instance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return newsarticle.Instance{}, f.err
	}
	for i, inst := range f.byUser[userID] {
		if inst.ID != id {
			continue
		}
		if inst.Answered {
			return inst, nil
		}
		inst.Answered = true
		inst.SelectedIndex = selectedIndex
		inst.Correct = selectedIndex == inst.Article.CorrectIndex
		f.byUser[userID][i] = inst
		return inst, nil
	}
	return newsarticle.Instance{}, nil
}

func (f *fakeArticleStore) Delete(ctx context.Context, userID, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	out := f.byUser[userID][:0]
	for _, inst := range f.byUser[userID] {
		if inst.ID != id {
			out = append(out, inst)
		}
	}
	f.byUser[userID] = out
	return nil
}

func (f *fakeArticleStore) Close() error { return nil }

// fakeArticleLLM is a minimal llm.Client double for GenerateArticleStudy.
type fakeArticleLLM struct {
	complete func(msgs []llm.Message) (string, error)
}

func (f *fakeArticleLLM) ChatStream(ctx context.Context, model string, msgs []llm.Message, onToken func(string)) (string, error) {
	return "", nil
}

func (f *fakeArticleLLM) Complete(ctx context.Context, model string, msgs []llm.Message, jsonMode bool) (string, error) {
	return f.complete(msgs)
}

func fakeArticlePipeline(raw string) *pipeline.Pipeline {
	return &pipeline.Pipeline{
		Analysis: []pipeline.Candidate{{Model: "m", LLM: &fakeArticleLLM{complete: func(msgs []llm.Message) (string, error) {
			return raw, nil
		}}}},
	}
}

const fakeStudyJSON = `{"summary":"A short English study paragraph.","choices":["정확한 해석","틀린 해석 1","틀린 해석 2","틀린 해석 3"],"correctIndex":0,"explanation":"정확한 해석이 원문의 의미를 담고 있기 때문입니다."}`

func fetchOneCandidate(c newsfeed.Candidate) func(context.Context) ([]newsfeed.Candidate, error) {
	return func(context.Context) ([]newsfeed.Candidate, error) {
		return []newsfeed.Candidate{c}, nil
	}
}

// ---- articleDrawHandler -----------------------------------------------------

func TestArticleDrawHandlerGeneratesAndCachesOnFirstDraw(t *testing.T) {
	st := &fakeArticleStore{}
	pipe := fakeArticlePipeline(fakeStudyJSON)
	fetch := fetchOneCandidate(newsfeed.Candidate{Source: "BBC", Title: "Headline", URL: "https://example.com/a", Description: "snippet"})
	h := articleDrawHandler(fakeIdentifier{id: "alex", ok: true}, st, pipe, fetch)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/api/articles/draw", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var got articleDraw
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Source != "BBC" || got.Summary == "" || len(got.Choices) != 4 {
		t.Fatalf("draw = %+v, want a populated draw", got)
	}
	// The answer key must never be sent in the draw response.
	if strings.Contains(rec.Body.String(), "correctIndex") || strings.Contains(rec.Body.String(), "explanation") {
		t.Fatalf("draw response leaked the answer key: %s", rec.Body.String())
	}
	if _, ok, _ := st.FindArticleByURL(context.Background(), "https://example.com/a"); !ok {
		t.Fatal("expected the article to be cached after generation")
	}
}

func TestArticleDrawHandlerReusesCachedArticleWithoutCallingLLM(t *testing.T) {
	st := &fakeArticleStore{byURL: map[string]newsarticle.Article{
		"https://example.com/a": {ID: "a1", Source: "BBC", Title: "Headline", URL: "https://example.com/a", Summary: "cached summary", Choices: []string{"a", "b", "c", "d"}, CorrectIndex: 1, Explanation: "e"},
	}}
	llmCalls := 0
	pipe := &pipeline.Pipeline{Analysis: []pipeline.Candidate{{Model: "m", LLM: &fakeArticleLLM{complete: func(msgs []llm.Message) (string, error) {
		llmCalls++
		return fakeStudyJSON, nil
	}}}}}
	fetch := fetchOneCandidate(newsfeed.Candidate{Source: "BBC", Title: "Headline", URL: "https://example.com/a", Description: "snippet"})
	h := articleDrawHandler(fakeIdentifier{id: "alex", ok: true}, st, pipe, fetch)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/api/articles/draw", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	if llmCalls != 0 {
		t.Fatalf("LLM calls = %d, want 0 (cached article should be reused)", llmCalls)
	}
	var got articleDraw
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Summary != "cached summary" {
		t.Fatalf("Summary = %q, want the cached summary reused", got.Summary)
	}
}

// TestArticleDrawHandlerExcludesAlreadyUsedArticles guards the core
// requirement: an article this learner already drew must never be offered
// again.
func TestArticleDrawHandlerExcludesAlreadyUsedArticles(t *testing.T) {
	st := &fakeArticleStore{
		byURL: map[string]newsarticle.Article{
			"https://example.com/used": {ID: "used", URL: "https://example.com/used"},
		},
		byUser: map[string][]newsarticle.Instance{
			"alex": {{ID: "i1", UserID: "alex", Article: newsarticle.Article{ID: "used", URL: "https://example.com/used"}}},
		},
	}
	pipe := fakeArticlePipeline(fakeStudyJSON)
	fetch := func(context.Context) ([]newsfeed.Candidate, error) {
		return []newsfeed.Candidate{{Source: "BBC", Title: "Old", URL: "https://example.com/used", Description: "d"}}, nil
	}
	h := articleDrawHandler(fakeIdentifier{id: "alex", ok: true}, st, pipe, fetch)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/api/articles/draw", nil))

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 when every candidate was already drawn", rec.Code)
	}
}

func TestArticleDrawHandlerUnauthorizedWhenIdentifyFails(t *testing.T) {
	h := articleDrawHandler(fakeIdentifier{ok: false}, &fakeArticleStore{}, fakeArticlePipeline(fakeStudyJSON), fetchOneCandidate(newsfeed.Candidate{URL: "https://example.com/a"}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/api/articles/draw", nil))

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestArticleDrawHandlerInternalErrorOnFetchFailure(t *testing.T) {
	fetch := func(context.Context) ([]newsfeed.Candidate, error) { return nil, errors.New("all feeds down") }
	h := articleDrawHandler(fakeIdentifier{id: "alex", ok: true}, &fakeArticleStore{}, fakeArticlePipeline(fakeStudyJSON), fetch)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/api/articles/draw", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

// ---- articleAnswerHandler ---------------------------------------------------

func postAnswerRequest(t *testing.T, id string, selectedIndex int) *http.Request {
	t.Helper()
	body, err := json.Marshal(map[string]int{"selectedIndex": selectedIndex})
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	req := httptest.NewRequest("POST", "/api/articles/"+id+"/answer", strings.NewReader(string(body)))
	req.SetPathValue("id", id)
	return req
}

func TestArticleAnswerHandlerComputesCorrectnessServerSide(t *testing.T) {
	st := &fakeArticleStore{byUser: map[string][]newsarticle.Instance{
		"alex": {{ID: "i1", UserID: "alex", SelectedIndex: -1, Article: newsarticle.Article{
			Choices: []string{"a", "b", "c", "d"}, CorrectIndex: 2, Explanation: "왜냐하면",
		}}},
	}}
	h := articleAnswerHandler(fakeIdentifier{id: "alex", ok: true}, st)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, postAnswerRequest(t, "i1", 2))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	var got articleResult
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.Correct || got.CorrectIndex != 2 || got.Translation != "c" || got.Explanation != "왜냐하면" {
		t.Fatalf("result = %+v, want Correct=true CorrectIndex=2 Translation=c", got)
	}
}

func TestArticleAnswerHandlerMarksWrongChoiceIncorrect(t *testing.T) {
	st := &fakeArticleStore{byUser: map[string][]newsarticle.Instance{
		"alex": {{ID: "i1", UserID: "alex", SelectedIndex: -1, Article: newsarticle.Article{
			Choices: []string{"a", "b", "c", "d"}, CorrectIndex: 2,
		}}},
	}}
	h := articleAnswerHandler(fakeIdentifier{id: "alex", ok: true}, st)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, postAnswerRequest(t, "i1", 0))

	var got articleResult
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Correct {
		t.Fatalf("result = %+v, want Correct=false for a wrong choice", got)
	}
}

func TestArticleAnswerHandlerNotFoundForMissingInstance(t *testing.T) {
	h := articleAnswerHandler(fakeIdentifier{id: "alex", ok: true}, &fakeArticleStore{})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, postAnswerRequest(t, "missing", 0))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestArticleAnswerHandlerUnauthorizedWhenIdentifyFails(t *testing.T) {
	h := articleAnswerHandler(fakeIdentifier{ok: false}, &fakeArticleStore{})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, postAnswerRequest(t, "i1", 0))

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

// ---- articleInstancesListHandler / articleDeleteHandler --------------------

func TestArticleInstancesListHandlerReturnsStoreResult(t *testing.T) {
	st := &fakeArticleStore{byUser: map[string][]newsarticle.Instance{
		"alex": {{ID: "i1", UserID: "alex", Article: newsarticle.Article{Source: "BBC", Title: "t", Summary: "s"}, CreatedAt: time.Now()}},
	}}
	h := articleInstancesListHandler(fakeIdentifier{id: "alex", ok: true}, st)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/articles", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got []articleListItem
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 1 || got[0].ID != "i1" || got[0].Source != "BBC" {
		t.Fatalf("got %+v", got)
	}
}

func TestArticleInstancesListHandlerInternalErrorOnStoreFailure(t *testing.T) {
	h := articleInstancesListHandler(fakeIdentifier{id: "alex", ok: true}, &fakeArticleStore{err: errors.New("mysql unreachable")})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/articles", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

func TestArticleDeleteHandlerRemovesInstance(t *testing.T) {
	st := &fakeArticleStore{byUser: map[string][]newsarticle.Instance{
		"alex": {{ID: "i1", UserID: "alex"}},
	}}
	h := articleDeleteHandler(fakeIdentifier{id: "alex", ok: true}, st)

	req := httptest.NewRequest("DELETE", "/api/articles/i1", nil)
	req.SetPathValue("id", "i1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	if len(st.byUser["alex"]) != 0 {
		t.Fatalf("instance still present after delete: %+v", st.byUser["alex"])
	}
}
