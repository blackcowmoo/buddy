package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"io"
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
	"buddy/server/internal/transport"
	"buddy/server/internal/ttsstore"
)

// fakeAudioSpeaker/fakeAudioCache are minimal tts.Speaker/ttsstore.Cache
// doubles for articleAudioHandler tests — mirror internal/transport's own
// fakeSpeaker/fakeCache (unexported there, so not reusable across packages).
type fakeAudioSpeaker struct {
	failWith error
	calls    int
}

func (f *fakeAudioSpeaker) Speak(ctx context.Context, text string) ([]byte, error) {
	f.calls++
	if f.failWith != nil {
		return nil, f.failWith
	}
	return []byte("audio-for:" + text), nil
}

func (f *fakeAudioSpeaker) Stream(ctx context.Context, text string) (io.ReadCloser, error) {
	audio, err := f.Speak(ctx, text)
	if err != nil {
		return nil, err
	}
	return io.NopCloser(strings.NewReader(string(audio))), nil
}

func (f *fakeAudioSpeaker) Version() string { return "test-version" }

// fakeAudioCache ignores the version passed to Put/Open — the real
// version-mismatch invalidation logic is ttsstore.Store's own, covered by
// its container-backed tests.
type fakeAudioCache struct {
	byKey map[string][]byte
}

func (f *fakeAudioCache) Put(ctx context.Context, key, version string, audio []byte) error {
	if f.byKey == nil {
		f.byKey = map[string][]byte{}
	}
	f.byKey[key] = audio
	return nil
}

func (f *fakeAudioCache) Open(ctx context.Context, key, version string) (io.ReadCloser, error) {
	audio, ok := f.byKey[key]
	if !ok {
		return nil, ttsstore.ErrNotFound
	}
	return io.NopCloser(strings.NewReader(string(audio))), nil
}

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

func (f *fakeArticleStore) ReserveArticle(ctx context.Context, source, title, url, description string, publishedAt time.Time) (newsarticle.Article, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return newsarticle.Article{}, f.err
	}
	if existing, ok := f.byURL[url]; ok {
		return existing, nil
	}
	a := newsarticle.Article{ID: "article-" + url, Source: source, Title: title, URL: url, Description: description, PublishedAt: publishedAt, Status: newsarticle.StatusPending, CreatedAt: time.Now()}
	if f.byURL == nil {
		f.byURL = map[string]newsarticle.Article{}
	}
	f.byURL[url] = a
	return a, nil
}

// StalePending/ClaimArticle are unused by these handler tests (the sweep is
// exercised in internal/transport's own tests) — minimal stubs to satisfy
// newsarticle.Store.
func (f *fakeArticleStore) StalePending(ctx context.Context, olderThan time.Duration) ([]newsarticle.Article, error) {
	return nil, nil
}

func (f *fakeArticleStore) ClaimArticle(ctx context.Context, id string) (bool, error) {
	return false, nil
}

func (f *fakeArticleStore) ReopenIncompleteArticle(ctx context.Context, id string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for url, a := range f.byURL {
		if a.ID != id || a.Status != newsarticle.StatusDone || len(a.SubQuestions) > 0 {
			continue
		}
		a.Status = newsarticle.StatusPending
		f.byURL[url] = a
		return true, nil
	}
	return false, nil
}

func (f *fakeArticleStore) CompleteArticle(ctx context.Context, id, summary, translation string, subQuestions []newsarticle.SubQuestion) (newsarticle.Article, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return newsarticle.Article{}, f.err
	}
	for url, a := range f.byURL {
		if a.ID != id || a.Status != newsarticle.StatusPending {
			continue
		}
		a.Summary = summary
		a.Translation = translation
		a.SubQuestions = subQuestions
		a.Status = newsarticle.StatusDone
		f.byURL[url] = a
		return a, nil
	}
	return newsarticle.Article{}, nil
}

func (f *fakeArticleStore) FailArticle(ctx context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	for url, a := range f.byURL {
		if a.ID == id && a.Status == newsarticle.StatusPending {
			a.Status = newsarticle.StatusFailed
			f.byURL[url] = a
		}
	}
	return nil
}

func (f *fakeArticleStore) GetArticle(ctx context.Context, id string) (newsarticle.Article, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return newsarticle.Article{}, false, f.err
	}
	for _, a := range f.byURL {
		if a.ID == id {
			return a, true, nil
		}
	}
	return newsarticle.Article{}, false, nil
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
		CreatedAt: time.Now(),
	}
	if f.byUser == nil {
		f.byUser = map[string][]newsarticle.Instance{}
	}
	f.byUser[userID] = append(f.byUser[userID], inst)
	return inst, nil
}

// refreshArticleLocked mirrors the real MySQL store's live join between
// buddy_article_instances and buddy_articles: an Instance's embedded Article
// always reflects the article row's *current* state (e.g. Status flipping
// pending -> done via CompleteArticle after the Instance was created), not a
// stale snapshot from CreateInstance time. Falls back to inst's own embedded
// Article unchanged if its ID isn't in f.byURL — several handler tests build
// an Instance{Article: ...} directly without ever calling ReserveArticle, so
// there's nothing to re-join against for those. Caller must hold f.mu.
func (f *fakeArticleStore) refreshArticleLocked(inst newsarticle.Instance) newsarticle.Instance {
	for _, a := range f.byURL {
		if a.ID == inst.Article.ID {
			inst.Article = a
			return inst
		}
	}
	return inst
}

func (f *fakeArticleStore) List(ctx context.Context, userID string) ([]newsarticle.Instance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	out := make([]newsarticle.Instance, len(f.byUser[userID]))
	for i, inst := range f.byUser[userID] {
		inst = f.refreshArticleLocked(inst)
		out[i] = inst
	}
	return out, nil
}

func (f *fakeArticleStore) Get(ctx context.Context, userID, id string) (newsarticle.Instance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return newsarticle.Instance{}, f.err
	}
	for _, inst := range f.byUser[userID] {
		if inst.ID == id {
			inst = f.refreshArticleLocked(inst)
			return inst, nil
		}
	}
	return newsarticle.Instance{}, nil
}

func (f *fakeArticleStore) Answer(ctx context.Context, userID, id string, selectedOptions []int) (newsarticle.Instance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return newsarticle.Instance{}, f.err
	}
	for i, inst := range f.byUser[userID] {
		if inst.ID != id {
			continue
		}
		inst = f.refreshArticleLocked(inst)
		if inst.Answered {
			return inst, nil
		}
		inst.Answered = true
		inst.SelectedOptions = selectedOptions
		correct := len(selectedOptions) == len(inst.Article.SubQuestions)
		for j, q := range inst.Article.SubQuestions {
			if j >= len(selectedOptions) || selectedOptions[j] != q.CorrectOptionIndex {
				correct = false
				break
			}
		}
		inst.Correct = correct
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

const fakeStudyJSON = `{"summary":"A short English study paragraph.","translation":"짧은 영어 학습 문단입니다.","subQuestions":[{"prompt":"어떤 내용이었나요?","options":["정확한 해석","틀린 해석"],"correctOptionIndex":0,"explanation":"정확한 해석이 원문의 의미를 담고 있기 때문입니다."},{"prompt":"언제 일어났나요?","options":["오늘","어제"],"correctOptionIndex":0,"explanation":"원문에 명시되어 있습니다."}]}`

func fetchOneCandidate(c newsfeed.Candidate) func(context.Context) ([]newsfeed.Candidate, error) {
	return func(context.Context) ([]newsfeed.Candidate, error) {
		return []newsfeed.Candidate{c}, nil
	}
}

// ---- articleDrawHandler -----------------------------------------------------

// TestArticleDrawHandlerReservesPendingAndCompletesInBackground guards the
// core async requirement: the draw response comes back immediately with
// Status "pending" — never blocking on the LLM call — and the article is
// generated (and cached) on a detached goroutine that outlives the request,
// via enqueueOrRunInline's inline fallback (articleStudyQueue is nil here,
// same as when Redis isn't configured).
func TestArticleDrawHandlerReservesPendingAndCompletesInBackground(t *testing.T) {
	st := &fakeArticleStore{}
	pipe := fakeArticlePipeline(fakeStudyJSON)
	fetch := fetchOneCandidate(newsfeed.Candidate{Source: "BBC", Title: "Headline", URL: "https://example.com/a", Description: "snippet"})
	h := articleDrawHandler(fakeIdentifier{id: "alex", ok: true}, st, pipe, fetch, nil, nil)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/api/articles/draw", nil))

	requireStatus(t, rec, http.StatusOK)
	var got articleDraw
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Source != "BBC" || got.Status != newsarticle.StatusPending || got.Summary != "" || len(got.SubQuestions) != 0 {
		t.Fatalf("draw = %+v, want an immediate pending draw with no study content yet", got)
	}
	// The answer key must never be sent in the draw response.
	if strings.Contains(rec.Body.String(), "correctOptionIndex") || strings.Contains(rec.Body.String(), "explanation") {
		t.Fatalf("draw response leaked the answer key: %s", rec.Body.String())
	}

	// got.ID is the Instance ID (toArticleDraw's shape) — re-fetch it to
	// learn the underlying Article's own ID to poll GetArticle by.
	waitForCondition(t, 2*time.Second, func() bool {
		inst, err := st.Get(context.Background(), "alex", got.ID)
		return err == nil && inst.Article.Status == newsarticle.StatusDone
	})
	inst, err := st.Get(context.Background(), "alex", got.ID)
	if err != nil || inst.Article.Summary == "" || len(inst.Article.SubQuestions) != 2 {
		t.Fatalf("Get() after background generation = (%+v, %v), want a completed article", inst, err)
	}
}

// TestArticleDrawHandlerCarriesPublishedAtFromTheCandidate guards that the
// feed's own pubDate (newsfeed.Candidate.PublishedAt) reaches the draw
// response immediately at reserve time — not just once generation finishes —
// since the reading view needs it to show a date even while status is still
// "pending".
func TestArticleDrawHandlerCarriesPublishedAtFromTheCandidate(t *testing.T) {
	st := &fakeArticleStore{}
	pipe := fakeArticlePipeline(fakeStudyJSON)
	published := time.Date(2024, 3, 15, 9, 30, 0, 0, time.UTC)
	fetch := fetchOneCandidate(newsfeed.Candidate{Source: "BBC", Title: "Headline", URL: "https://example.com/a", Description: "snippet", PublishedAt: published})
	h := articleDrawHandler(fakeIdentifier{id: "alex", ok: true}, st, pipe, fetch, nil, nil)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/api/articles/draw", nil))

	var got articleDraw
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.PublishedAt != published.Unix() {
		t.Fatalf("PublishedAt = %d, want %d", got.PublishedAt, published.Unix())
	}
}

func TestArticleDrawHandlerReusesCachedArticleWithoutCallingLLM(t *testing.T) {
	st := &fakeArticleStore{byURL: map[string]newsarticle.Article{
		"https://example.com/a": {ID: "a1", Source: "BBC", Title: "Headline", URL: "https://example.com/a", Summary: "cached summary", SubQuestions: []newsarticle.SubQuestion{
			{Prompt: "p1", Options: []string{"a", "b"}, CorrectOptionIndex: 1, Explanation: "e"},
			{Prompt: "p2", Options: []string{"c", "d"}, CorrectOptionIndex: 0, Explanation: "e"},
		}, Status: newsarticle.StatusDone},
	}}
	llmCalls := 0
	pipe := &pipeline.Pipeline{Analysis: []pipeline.Candidate{{Model: "m", LLM: &fakeArticleLLM{complete: func(msgs []llm.Message) (string, error) {
		llmCalls++
		return fakeStudyJSON, nil
	}}}}}
	fetch := fetchOneCandidate(newsfeed.Candidate{Source: "BBC", Title: "Headline", URL: "https://example.com/a", Description: "snippet"})
	h := articleDrawHandler(fakeIdentifier{id: "alex", ok: true}, st, pipe, fetch, nil, nil)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/api/articles/draw", nil))

	requireStatus(t, rec, http.StatusOK)
	var got articleDraw
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Summary != "cached summary" || got.Status != newsarticle.StatusDone {
		t.Fatalf("draw = %+v, want the cached, already-done article reused", got)
	}
	// Nothing gets enqueued for an already-StatusDone article, but the
	// background fallback goroutine (if any were spawned) would still race
	// this assertion — give it a moment to prove it never calls the LLM.
	time.Sleep(50 * time.Millisecond)
	if llmCalls != 0 {
		t.Fatalf("LLM calls = %d, want 0 (cached article should be reused)", llmCalls)
	}
}

// TestArticleDrawHandlerRegeneratesAPreMigrationDoneArticleWithNoSubQuestions
// guards the draw-side half of the same self-heal fix as
// TestArticleInstanceHandlerSelfHealsAPreMigrationDoneArticleWithNoSubQuestions:
// re-drawing a URL whose cached Article is StatusDone but has no
// SubQuestions (left over from before the 4-choice -> N-sub-question quiz
// redesign) must trigger real regeneration instead of just handing back the
// same permanently-broken cached row.
func TestArticleDrawHandlerRegeneratesAPreMigrationDoneArticleWithNoSubQuestions(t *testing.T) {
	st := &fakeArticleStore{byURL: map[string]newsarticle.Article{
		"https://example.com/a": {ID: "a1", Source: "BBC", Title: "Headline", URL: "https://example.com/a", Summary: "stale summary", Status: newsarticle.StatusDone},
	}}
	llmCalls := 0
	pipe := &pipeline.Pipeline{Analysis: []pipeline.Candidate{{Model: "m", LLM: &fakeArticleLLM{complete: func(msgs []llm.Message) (string, error) {
		llmCalls++
		return fakeStudyJSON, nil
	}}}}}
	fetch := fetchOneCandidate(newsfeed.Candidate{Source: "BBC", Title: "Headline", URL: "https://example.com/a", Description: "snippet"})
	h := articleDrawHandler(fakeIdentifier{id: "alex", ok: true}, st, pipe, fetch, nil, nil)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/api/articles/draw", nil))

	requireStatus(t, rec, http.StatusOK)
	var got articleDraw
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Status != newsarticle.StatusPending {
		t.Fatalf("draw = %+v, want status %q (self-healed back to pending)", got, newsarticle.StatusPending)
	}

	waitForCondition(t, 2*time.Second, func() bool {
		a, ok, err := st.GetArticle(context.Background(), "a1")
		return err == nil && ok && a.Status == newsarticle.StatusDone
	})
	if llmCalls != 1 {
		t.Fatalf("LLM calls = %d, want exactly 1", llmCalls)
	}
}

// TestArticleDrawHandlerPicksTheMostRecentCandidate guards the ordering
// contract with newsfeed.FetchCandidates (which sorts newest-first): the
// handler must take fetchCandidates' first not-yet-drawn entry as-is rather
// than picking randomly among them.
func TestArticleDrawHandlerPicksTheMostRecentCandidate(t *testing.T) {
	st := &fakeArticleStore{}
	pipe := fakeArticlePipeline(fakeStudyJSON)
	fetch := func(context.Context) ([]newsfeed.Candidate, error) {
		return []newsfeed.Candidate{
			{Source: "BBC", Title: "Newest", URL: "https://example.com/newest", Description: "d"},
			{Source: "NPR", Title: "Older", URL: "https://example.com/older", Description: "d"},
		}, nil
	}
	h := articleDrawHandler(fakeIdentifier{id: "alex", ok: true}, st, pipe, fetch, nil, nil)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/api/articles/draw", nil))

	requireStatus(t, rec, http.StatusOK)
	var got articleDraw
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Title != "Newest" {
		t.Fatalf("draw picked %q, want the first (most recent) candidate", got.Title)
	}
}

// TestArticleDrawHandlerExcludesAlreadyUsedArticles guards the core
// requirement: an article this learner already drew must never be offered
// again.
func TestArticleDrawHandlerExcludesAlreadyUsedArticles(t *testing.T) {
	st := &fakeArticleStore{
		byURL: map[string]newsarticle.Article{
			"https://example.com/used": {ID: "used", URL: "https://example.com/used", Status: newsarticle.StatusDone},
		},
		byUser: map[string][]newsarticle.Instance{
			"alex": {{ID: "i1", UserID: "alex", Article: newsarticle.Article{ID: "used", URL: "https://example.com/used"}}},
		},
	}
	pipe := fakeArticlePipeline(fakeStudyJSON)
	fetch := func(context.Context) ([]newsfeed.Candidate, error) {
		return []newsfeed.Candidate{{Source: "BBC", Title: "Old", URL: "https://example.com/used", Description: "d"}}, nil
	}
	h := articleDrawHandler(fakeIdentifier{id: "alex", ok: true}, st, pipe, fetch, nil, nil)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/api/articles/draw", nil))

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 when every candidate was already drawn", rec.Code)
	}
}

func TestArticleDrawHandlerUnauthorizedWhenIdentifyFails(t *testing.T) {
	h := articleDrawHandler(fakeIdentifier{ok: false}, &fakeArticleStore{}, fakeArticlePipeline(fakeStudyJSON), fetchOneCandidate(newsfeed.Candidate{URL: "https://example.com/a"}), nil, nil)

	assertUnauthorized(t, h, httptest.NewRequest("POST", "/api/articles/draw", nil))
}

func TestArticleDrawHandlerInternalErrorOnFetchFailure(t *testing.T) {
	fetch := func(context.Context) ([]newsfeed.Candidate, error) { return nil, errors.New("all feeds down") }
	h := articleDrawHandler(fakeIdentifier{id: "alex", ok: true}, &fakeArticleStore{}, fakeArticlePipeline(fakeStudyJSON), fetch, nil, nil)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/api/articles/draw", nil))

	requireStatus(t, rec, http.StatusInternalServerError)
}

// ---- articleAnswerHandler ---------------------------------------------------

func postAnswerRequest(t *testing.T, id string, selectedOptions []int) *http.Request {
	t.Helper()
	body, err := json.Marshal(map[string][]int{"selectedOptions": selectedOptions})
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	req := httptest.NewRequest("POST", "/api/articles/"+id+"/answer", strings.NewReader(string(body)))
	req.SetPathValue("id", id)
	return req
}

func TestArticleAnswerHandlerComputesCorrectnessServerSide(t *testing.T) {
	st := &fakeArticleStore{byUser: map[string][]newsarticle.Instance{
		"alex": {{ID: "i1", UserID: "alex", Article: newsarticle.Article{
			Translation: "영어 원문의 한글 번역입니다.",
			SubQuestions: []newsarticle.SubQuestion{
				{Prompt: "p1", Options: []string{"a", "b"}, CorrectOptionIndex: 1, Explanation: "왜냐하면1"},
				{Prompt: "p2", Options: []string{"c", "d"}, CorrectOptionIndex: 0, Explanation: "왜냐하면2"},
			},
			Status: newsarticle.StatusDone,
		}}},
	}}
	h := articleAnswerHandler(fakeIdentifier{id: "alex", ok: true}, st)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, postAnswerRequest(t, "i1", []int{1, 0}))

	requireStatus(t, rec, http.StatusOK)
	var got articleResult
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.Correct || got.Score != 2 || got.Total != 2 || got.Translation != "영어 원문의 한글 번역입니다." {
		t.Fatalf("result = %+v, want Correct=true Score=2 Total=2", got)
	}
	if len(got.SubQuestions) != 2 || !got.SubQuestions[0].Correct || got.SubQuestions[0].Explanation != "왜냐하면1" {
		t.Fatalf("result.SubQuestions = %+v, want both sub-questions revealed correct", got.SubQuestions)
	}
}

func TestArticleAnswerHandlerMarksWrongChoiceIncorrect(t *testing.T) {
	st := &fakeArticleStore{byUser: map[string][]newsarticle.Instance{
		"alex": {{ID: "i1", UserID: "alex", Article: newsarticle.Article{
			SubQuestions: []newsarticle.SubQuestion{
				{Prompt: "p1", Options: []string{"a", "b"}, CorrectOptionIndex: 1, Explanation: "e"},
				{Prompt: "p2", Options: []string{"c", "d"}, CorrectOptionIndex: 0, Explanation: "e"},
			},
			Status: newsarticle.StatusDone,
		}}},
	}}
	h := articleAnswerHandler(fakeIdentifier{id: "alex", ok: true}, st)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, postAnswerRequest(t, "i1", []int{0, 0}))

	var got articleResult
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Correct || got.Score != 1 {
		t.Fatalf("result = %+v, want Correct=false Score=1 for one wrong sub-question", got)
	}
}

// TestArticleAnswerHandlerConflictWhileStillPending guards the panic-avoidance
// guard: an instance whose Article hasn't finished generating yet
// (SubQuestions is empty) must be rejected with 409, never reach
// Store.Answer/toArticleResult's SubQuestions[i].CorrectOptionIndex index.
func TestArticleAnswerHandlerConflictWhileStillPending(t *testing.T) {
	st := &fakeArticleStore{byUser: map[string][]newsarticle.Instance{
		"alex": {{ID: "i1", UserID: "alex", Article: newsarticle.Article{Status: newsarticle.StatusPending}}},
	}}
	h := articleAnswerHandler(fakeIdentifier{id: "alex", ok: true}, st)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, postAnswerRequest(t, "i1", nil))

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 for a still-pending article", rec.Code)
	}
}

func TestArticleAnswerHandlerNotFoundForMissingInstance(t *testing.T) {
	h := articleAnswerHandler(fakeIdentifier{id: "alex", ok: true}, &fakeArticleStore{})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, postAnswerRequest(t, "missing", nil))

	requireStatus(t, rec, http.StatusNotFound)
}

func TestArticleAnswerHandlerUnauthorizedWhenIdentifyFails(t *testing.T) {
	h := articleAnswerHandler(fakeIdentifier{ok: false}, &fakeArticleStore{})

	assertUnauthorized(t, h, postAnswerRequest(t, "i1", nil))
}

// ---- articleInstanceHandler --------------------------------------------------

// TestArticleInstanceHandlerReturnsCurrentStatus guards the poll contract: a
// learner who navigated away mid-generation and comes back can re-fetch the
// same draw and see its current status, exactly like articleDrawHandler's
// own response shape.
func TestArticleInstanceHandlerReturnsCurrentStatus(t *testing.T) {
	st := &fakeArticleStore{byUser: map[string][]newsarticle.Instance{
		"alex": {{ID: "i1", UserID: "alex", Article: newsarticle.Article{Source: "BBC", Title: "t", Status: newsarticle.StatusPending}, CreatedAt: time.Now()}},
	}}
	h := articleInstanceHandler(fakeIdentifier{id: "alex", ok: true}, st, fakeArticlePipeline(fakeStudyJSON), nil, nil)

	req := httptest.NewRequest("GET", "/api/articles/i1", nil)
	req.SetPathValue("id", "i1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	requireStatus(t, rec, http.StatusOK)
	var got articleDraw
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Status != newsarticle.StatusPending {
		t.Fatalf("Status = %q, want %q", got.Status, newsarticle.StatusPending)
	}
}

// TestArticleInstanceHandlerSelfHealsAPreMigrationDoneArticleWithNoSubQuestions
// guards the fix for a real reported bug: reopening a past draw whose
// Article was left StatusDone with no SubQuestions — the shape a row
// completed before the 4-choice -> N-sub-question quiz redesign is stuck in
// — must not show a permanently blank quiz. It should transition back to
// "pending" and kick off real generation exactly once (the same "생성 중"
// state a fresh draw shows), not re-enqueue on every subsequent poll of an
// article that's now legitimately generating (see selfHealIncompleteArticle's
// doc comment on why the enqueue is gated on winning the reopen, not just on
// Status == "pending").
func TestArticleInstanceHandlerSelfHealsAPreMigrationDoneArticleWithNoSubQuestions(t *testing.T) {
	stale := newsarticle.Article{
		ID: "a1", Source: "BBC", Title: "Old", URL: "https://example.com/old",
		Description: "old snippet", Status: newsarticle.StatusDone, Summary: "stale summary",
	}
	st := &fakeArticleStore{
		byURL:  map[string]newsarticle.Article{stale.URL: stale},
		byUser: map[string][]newsarticle.Instance{"alex": {{ID: "i1", UserID: "alex", Article: stale, CreatedAt: time.Now()}}},
	}
	calls := 0
	pipe := &pipeline.Pipeline{Analysis: []pipeline.Candidate{{Model: "m", LLM: &fakeArticleLLM{complete: func(msgs []llm.Message) (string, error) {
		calls++
		return fakeStudyJSON, nil
	}}}}}
	h := articleInstanceHandler(fakeIdentifier{id: "alex", ok: true}, st, pipe, nil, nil)

	req := httptest.NewRequest("GET", "/api/articles/i1", nil)
	req.SetPathValue("id", "i1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	requireStatus(t, rec, http.StatusOK)
	var got articleDraw
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Status != newsarticle.StatusPending {
		t.Fatalf("Status = %q, want %q (self-healed back to pending)", got.Status, newsarticle.StatusPending)
	}

	// The regeneration itself runs on a detached goroutine (EnqueueOrRunInline's
	// inline fallback, since articleStudyQueue is nil here) — give it a moment
	// to actually land.
	waitForCondition(t, 2*time.Second, func() bool {
		inst, err := st.Get(context.Background(), "alex", "i1")
		return err == nil && inst.Article.Status == newsarticle.StatusDone
	})
	if calls != 1 {
		t.Fatalf("LLM calls = %d, want exactly 1", calls)
	}

	// A second poll, now that generation has landed, must not re-trigger it.
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req)
	requireStatus(t, rec2, http.StatusOK)
	if calls != 1 {
		t.Fatalf("LLM calls after a second poll = %d, want still 1 (no re-enqueue for an already-done article)", calls)
	}
}

func TestArticleInstanceHandlerNotFoundForMissingOrOtherUsersInstance(t *testing.T) {
	h := articleInstanceHandler(fakeIdentifier{id: "alex", ok: true}, &fakeArticleStore{}, fakeArticlePipeline(fakeStudyJSON), nil, nil)

	req := httptest.NewRequest("GET", "/api/articles/missing", nil)
	req.SetPathValue("id", "missing")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	requireStatus(t, rec, http.StatusNotFound)
}

func TestArticleInstanceHandlerUnauthorizedWhenIdentifyFails(t *testing.T) {
	h := articleInstanceHandler(fakeIdentifier{ok: false}, &fakeArticleStore{}, fakeArticlePipeline(fakeStudyJSON), nil, nil)

	req := httptest.NewRequest("GET", "/api/articles/i1", nil)
	req.SetPathValue("id", "i1")
	assertUnauthorized(t, h, req)
}

// ---- articleAudioHandler -----------------------------------------------------

func TestArticleAudioHandlerServesCachedAudioWithoutGenerating(t *testing.T) {
	st := &fakeArticleStore{byUser: map[string][]newsarticle.Instance{
		"alex": {{ID: "i1", UserID: "alex", Article: newsarticle.Article{ID: "a1", Status: newsarticle.StatusDone, Summary: "hello"}, CreatedAt: time.Now()}},
	}}
	speaker := &fakeAudioSpeaker{}
	cache := &fakeAudioCache{byKey: map[string][]byte{transport.ArticleAudioKey("a1", "hello"): []byte("cached-mp3-bytes")}}
	h := articleAudioHandler(fakeIdentifier{id: "alex", ok: true}, st, &transport.ArticleAudio{Client: speaker, Cache: cache})

	req := httptest.NewRequest("GET", "/api/articles/i1/audio", nil)
	req.SetPathValue("id", "i1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	requireStatus(t, rec, http.StatusOK)
	if rec.Body.String() != "cached-mp3-bytes" {
		t.Fatalf("body = %q, want the cached bytes served as-is", rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "audio/mpeg" {
		t.Errorf("Content-Type = %q, want audio/mpeg", ct)
	}
	if speaker.calls != 0 {
		t.Fatalf("Speak() called %d times, want 0 — a cache hit must never regenerate", speaker.calls)
	}
}

// TestArticleAudioHandlerGeneratesOnCacheMiss guards the "regenerate on
// next read" half of the TTL spec: never generated yet (or swept past its
// TTL — same ttsstore.ErrNotFound either way) falls back to generating on
// the spot instead of 404ing.
func TestArticleAudioHandlerGeneratesOnCacheMiss(t *testing.T) {
	st := &fakeArticleStore{byUser: map[string][]newsarticle.Instance{
		"alex": {{ID: "i1", UserID: "alex", Article: newsarticle.Article{ID: "a1", Status: newsarticle.StatusDone, Summary: "hello there"}, CreatedAt: time.Now()}},
	}}
	speaker := &fakeAudioSpeaker{}
	cache := &fakeAudioCache{}
	h := articleAudioHandler(fakeIdentifier{id: "alex", ok: true}, st, &transport.ArticleAudio{Client: speaker, Cache: cache})

	req := httptest.NewRequest("GET", "/api/articles/i1/audio", nil)
	req.SetPathValue("id", "i1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	requireStatus(t, rec, http.StatusOK)
	if rec.Body.String() != "audio-for:hello there" {
		t.Fatalf("body = %q, want freshly generated audio for the article's summary", rec.Body.String())
	}
	if speaker.calls != 1 {
		t.Fatalf("Speak() called %d times, want exactly 1", speaker.calls)
	}
	if _, ok := cache.byKey[transport.ArticleAudioKey("a1", "hello there")]; !ok {
		t.Fatal("freshly generated audio was not cached for the next request")
	}
}

func TestArticleAudioHandlerServiceUnavailableWhenTTSNotConfigured(t *testing.T) {
	st := &fakeArticleStore{byUser: map[string][]newsarticle.Instance{
		"alex": {{ID: "i1", UserID: "alex", Article: newsarticle.Article{ID: "a1", Status: newsarticle.StatusDone, Summary: "hello"}, CreatedAt: time.Now()}},
	}}
	h := articleAudioHandler(fakeIdentifier{id: "alex", ok: true}, st, nil)

	req := httptest.NewRequest("GET", "/api/articles/i1/audio", nil)
	req.SetPathValue("id", "i1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	requireStatus(t, rec, http.StatusServiceUnavailable)
}

func TestArticleAudioHandlerConflictWhileStillPending(t *testing.T) {
	st := &fakeArticleStore{byUser: map[string][]newsarticle.Instance{
		"alex": {{ID: "i1", UserID: "alex", Article: newsarticle.Article{ID: "a1", Status: newsarticle.StatusPending}, CreatedAt: time.Now()}},
	}}
	h := articleAudioHandler(fakeIdentifier{id: "alex", ok: true}, st, &transport.ArticleAudio{Client: &fakeAudioSpeaker{}, Cache: &fakeAudioCache{}})

	req := httptest.NewRequest("GET", "/api/articles/i1/audio", nil)
	req.SetPathValue("id", "i1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	requireStatus(t, rec, http.StatusConflict)
}

func TestArticleAudioHandlerNotFoundForMissingOrOtherUsersInstance(t *testing.T) {
	h := articleAudioHandler(fakeIdentifier{id: "alex", ok: true}, &fakeArticleStore{}, &transport.ArticleAudio{Client: &fakeAudioSpeaker{}, Cache: &fakeAudioCache{}})

	req := httptest.NewRequest("GET", "/api/articles/missing/audio", nil)
	req.SetPathValue("id", "missing")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	requireStatus(t, rec, http.StatusNotFound)
}

func TestArticleAudioHandlerUnauthorizedWhenIdentifyFails(t *testing.T) {
	h := articleAudioHandler(fakeIdentifier{ok: false}, &fakeArticleStore{}, &transport.ArticleAudio{Client: &fakeAudioSpeaker{}, Cache: &fakeAudioCache{}})

	req := httptest.NewRequest("GET", "/api/articles/i1/audio", nil)
	req.SetPathValue("id", "i1")
	assertUnauthorized(t, h, req)
}

func TestArticleAudioHandlerGenerationFailureIsServerError(t *testing.T) {
	st := &fakeArticleStore{byUser: map[string][]newsarticle.Instance{
		"alex": {{ID: "i1", UserID: "alex", Article: newsarticle.Article{ID: "a1", Status: newsarticle.StatusDone, Summary: "hello"}, CreatedAt: time.Now()}},
	}}
	h := articleAudioHandler(fakeIdentifier{id: "alex", ok: true}, st, &transport.ArticleAudio{
		Client: &fakeAudioSpeaker{failWith: errors.New("tts unreachable")},
		Cache:  &fakeAudioCache{},
	})

	req := httptest.NewRequest("GET", "/api/articles/i1/audio", nil)
	req.SetPathValue("id", "i1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	requireStatus(t, rec, http.StatusInternalServerError)
}

// ---- articleInstancesListHandler / articleDeleteHandler --------------------

func TestArticleInstancesListHandlerReturnsStoreResult(t *testing.T) {
	st := &fakeArticleStore{byUser: map[string][]newsarticle.Instance{
		"alex": {{ID: "i1", UserID: "alex", Article: newsarticle.Article{Source: "BBC", Title: "t", Summary: "s"}, CreatedAt: time.Now()}},
	}}
	h := articleInstancesListHandler(fakeIdentifier{id: "alex", ok: true}, st)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/articles", nil))

	requireStatus(t, rec, http.StatusOK)
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

	requireStatus(t, rec, http.StatusInternalServerError)
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

	requireStatus(t, rec, http.StatusNoContent)
	if len(st.byUser["alex"]) != 0 {
		t.Fatalf("instance still present after delete: %+v", st.byUser["alex"])
	}
}
