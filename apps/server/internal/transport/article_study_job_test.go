package transport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"buddy/server/internal/asyncjob"
	"buddy/server/internal/newsarticle"
	"buddy/server/internal/pipeline"
	"buddy/server/internal/ttsstore"
)

// fakeSpeaker is a minimal tts.Speaker double — succeeds unless failWith is
// set, and records every text it was asked to speak.
type fakeSpeaker struct {
	mu       sync.Mutex
	failWith error
	spoken   []string
}

func (f *fakeSpeaker) Speak(ctx context.Context, text string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failWith != nil {
		return nil, f.failWith
	}
	f.spoken = append(f.spoken, text)
	return []byte("audio-for:" + text), nil
}

func (f *fakeSpeaker) Stream(ctx context.Context, text string) (io.ReadCloser, error) {
	audio, err := f.Speak(ctx, text)
	if err != nil {
		return nil, err
	}
	return io.NopCloser(strings.NewReader(string(audio))), nil
}

func (f *fakeSpeaker) Version() string { return "test-version" }

func (f *fakeSpeaker) spokenTexts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.spoken...)
}

// fakeCache is a minimal ttsstore.Cache double, backed by an in-memory map.
// Ignores the version passed to Put/Open (the real version-mismatch
// invalidation logic is ttsstore.Store's own, covered by its
// container-backed tests) — just records the last one seen, for tests that
// want to assert the caller passed the right one.
type fakeCache struct {
	mu          sync.Mutex
	byKey       map[string][]byte
	putErr      error
	putCall     int
	lastVersion string
}

func (f *fakeCache) Put(ctx context.Context, key, version string, audio []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.putCall++
	f.lastVersion = version
	if f.putErr != nil {
		return f.putErr
	}
	if f.byKey == nil {
		f.byKey = map[string][]byte{}
	}
	f.byKey[key] = audio
	return nil
}

func (f *fakeCache) Open(ctx context.Context, key, version string) (io.ReadCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastVersion = version
	audio, ok := f.byKey[key]
	if !ok {
		return nil, ttsstore.ErrNotFound
	}
	return io.NopCloser(strings.NewReader(string(audio))), nil
}

func (f *fakeCache) putCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.putCall
}

// fakeNewsArticleStore is a minimal in-memory newsarticle.Store for these job
// tests — real SQL behavior is covered by internal/newsarticle's own tests.
// Guarded by mu since EnqueueArticleStudyJob's background path (and this
// package's own concurrent job execution) can touch it from more than one
// goroutine.
type fakeNewsArticleStore struct {
	mu       sync.Mutex
	articles map[string]newsarticle.Article // by id
	// stale tracks which article IDs StalePending should currently report —
	// a minimal, non-time-based stand-in for the real claimed_at column,
	// since these tests want to control staleness directly rather than
	// racing a real clock. A fresh reservation is never stale (mirrors
	// MySQLStore.ReserveArticle setting claimed_at = created_at); tests that
	// want to simulate an abandoned job call markStale.
	stale map[string]bool
}

func newFakeNewsArticleStore(articles ...newsarticle.Article) *fakeNewsArticleStore {
	s := &fakeNewsArticleStore{articles: map[string]newsarticle.Article{}, stale: map[string]bool{}}
	for _, a := range articles {
		s.articles[a.ID] = a
	}
	return s
}

func (s *fakeNewsArticleStore) ReserveArticle(ctx context.Context, source, title, url, description string, publishedAt time.Time) (newsarticle.Article, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.articles {
		if a.URL == url {
			return a, nil
		}
	}
	a := newsarticle.Article{ID: uuid.New().String(), Source: source, Title: title, URL: url, Description: description, PublishedAt: publishedAt, Status: newsarticle.StatusPending, CreatedAt: time.Now()}
	s.articles[a.ID] = a
	return a, nil
}

// markStale simulates an abandoned job — a StatusPending article whose
// generation attempt died without ever calling CompleteArticle/FailArticle —
// so sweep tests can make StalePending report it without waiting out a real
// ArticleStudyStaleAfter window.
func (s *fakeNewsArticleStore) markStale(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stale[id] = true
}

func (s *fakeNewsArticleStore) StalePending(ctx context.Context, olderThan time.Duration) ([]newsarticle.Article, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []newsarticle.Article
	for id, a := range s.articles {
		if a.Status == newsarticle.StatusPending && s.stale[id] {
			out = append(out, a)
		}
	}
	return out, nil
}

func (s *fakeNewsArticleStore) ClaimArticle(ctx context.Context, id string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.articles[id]
	if !ok || a.Status != newsarticle.StatusPending {
		return false, nil
	}
	delete(s.stale, id) // freshly (re)claimed, so no longer stale
	return true, nil
}

func (s *fakeNewsArticleStore) ReopenIncompleteArticle(ctx context.Context, id string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.articles[id]
	if !ok || a.Status != newsarticle.StatusDone || len(a.SubQuestions) > 0 {
		return false, nil
	}
	a.Status = newsarticle.StatusPending
	s.articles[id] = a
	return true, nil
}

func (s *fakeNewsArticleStore) CompleteArticle(ctx context.Context, id, summary string, subQuestions []newsarticle.SubQuestion) (newsarticle.Article, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.articles[id]
	if !ok || a.Status != newsarticle.StatusPending {
		return a, nil
	}
	a.Summary = summary
	a.SubQuestions = subQuestions
	a.Status = newsarticle.StatusDone
	s.articles[id] = a
	return a, nil
}

func (s *fakeNewsArticleStore) FailArticle(ctx context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.articles[id]
	if !ok || a.Status != newsarticle.StatusPending {
		return nil
	}
	a.Status = newsarticle.StatusFailed
	s.articles[id] = a
	return nil
}

func (s *fakeNewsArticleStore) GetArticle(ctx context.Context, id string) (newsarticle.Article, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.articles[id]
	return a, ok, nil
}

func (s *fakeNewsArticleStore) UsedURLs(ctx context.Context, userID string) (map[string]bool, error) {
	return nil, nil
}
func (s *fakeNewsArticleStore) CreateInstance(ctx context.Context, userID, articleID string) (newsarticle.Instance, error) {
	return newsarticle.Instance{}, nil
}
func (s *fakeNewsArticleStore) List(ctx context.Context, userID string) ([]newsarticle.Instance, error) {
	return nil, nil
}
func (s *fakeNewsArticleStore) Get(ctx context.Context, userID, id string) (newsarticle.Instance, error) {
	return newsarticle.Instance{}, nil
}
func (s *fakeNewsArticleStore) Answer(ctx context.Context, userID, id string, selectedOptions []int) (newsarticle.Instance, error) {
	return newsarticle.Instance{}, nil
}
func (s *fakeNewsArticleStore) Delete(ctx context.Context, userID, id string) error { return nil }
func (s *fakeNewsArticleStore) Close() error                                        { return nil }

func (s *fakeNewsArticleStore) status(id string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.articles[id].Status
}

const fakeArticleStudyJSON = `{"summary":"A short English study paragraph.","subQuestions":[{"prompt":"어떤 내용이었나요?","options":["정확한 해석","틀린 해석"],"correctOptionIndex":0,"explanation":"정확한 해석이 원문의 의미를 담고 있기 때문입니다."},{"prompt":"언제 일어났나요?","options":["오늘","어제"],"correctOptionIndex":0,"explanation":"원문에 명시되어 있습니다."}]}`

// TestRunArticleStudyCompletesAPendingArticle guards the primary flow: a
// StatusPending article whose LLM call succeeds ends up StatusDone with the
// generated content persisted.
func TestRunArticleStudyCompletesAPendingArticle(t *testing.T) {
	pipe := &pipeline.Pipeline{
		Analysis: []pipeline.Candidate{{Model: "m", LLM: fakeAnalysisLLM{complete: fakeArticleStudyJSON}}},
	}
	articles := newFakeNewsArticleStore(newsarticle.Article{ID: "a1", Source: "BBC", Title: "Headline", URL: "https://example.com/a", Status: newsarticle.StatusPending})

	if err := RunArticleStudyInline(context.Background(), pipe, articles, nil, "a1", "BBC", "Headline", "snippet"); err != nil {
		t.Fatalf("RunArticleStudyInline() error = %v", err)
	}
	if got := articles.status("a1"); got != newsarticle.StatusDone {
		t.Fatalf("status = %q, want %q", got, newsarticle.StatusDone)
	}
}

// TestRunArticleStudyGeneratesAndCachesReadAloudAudio guards the whole
// point of moving TTS server-side: a completed article's summary gets
// synthesized and cached once, under the article-ID-scoped key every future
// learner's audio request (see httpserver's article audio handler) will
// look up — not per learner, per Instance.
func TestRunArticleStudyGeneratesAndCachesReadAloudAudio(t *testing.T) {
	pipe := &pipeline.Pipeline{
		Analysis: []pipeline.Candidate{{Model: "m", LLM: fakeAnalysisLLM{complete: fakeArticleStudyJSON}}},
	}
	articles := newFakeNewsArticleStore(newsarticle.Article{ID: "a1", Source: "BBC", Title: "Headline", URL: "https://example.com/a", Status: newsarticle.StatusPending})
	speaker := &fakeSpeaker{}
	cache := &fakeCache{}
	audio := &ArticleAudio{Client: speaker, Cache: cache}

	if err := RunArticleStudyInline(context.Background(), pipe, articles, audio, "a1", "BBC", "Headline", "snippet"); err != nil {
		t.Fatalf("RunArticleStudyInline() error = %v", err)
	}

	wantText := "A short English study paragraph."
	if got := speaker.spokenTexts(); len(got) != 1 || got[0] != wantText {
		t.Fatalf("spoken texts = %v, want exactly [%q]", got, wantText)
	}
	rc, err := cache.Open(context.Background(), ArticleAudioKey("a1"), speaker.Version())
	if err != nil {
		t.Fatalf("cache.Open(%q) error = %v, want the generated audio to be cached under that key", ArticleAudioKey("a1"), err)
	}
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	if string(got) != "audio-for:"+wantText {
		t.Fatalf("cached audio = %q, want %q", got, "audio-for:"+wantText)
	}
}

// TestRunArticleStudySkipsGenerationWhenAudioIsNil guards the "BUDDY_TTS_URL
// unset" default: no TTS call, no cache write, and — most importantly —
// the article study job itself still succeeds.
func TestRunArticleStudySkipsGenerationWhenAudioIsNil(t *testing.T) {
	pipe := &pipeline.Pipeline{
		Analysis: []pipeline.Candidate{{Model: "m", LLM: fakeAnalysisLLM{complete: fakeArticleStudyJSON}}},
	}
	articles := newFakeNewsArticleStore(newsarticle.Article{ID: "a1", Source: "BBC", Title: "Headline", URL: "https://example.com/a", Status: newsarticle.StatusPending})

	if err := RunArticleStudyInline(context.Background(), pipe, articles, nil, "a1", "BBC", "Headline", "snippet"); err != nil {
		t.Fatalf("RunArticleStudyInline() error = %v, want nil audio to be a no-op, not a failure", err)
	}
	if got := articles.status("a1"); got != newsarticle.StatusDone {
		t.Fatalf("status = %q, want %q (summary/quiz still succeed without TTS)", got, newsarticle.StatusDone)
	}
}

// TestRunArticleStudySucceedsEvenWhenTTSGenerationFails guards the
// "best-effort" contract: read-aloud is a bonus, not a requirement — a
// failed Speak() call must never fail (or retry) the article study job
// itself, since httpserver's audio handler covers a still-missing cache
// entry by generating on demand later.
func TestRunArticleStudySucceedsEvenWhenTTSGenerationFails(t *testing.T) {
	pipe := &pipeline.Pipeline{
		Analysis: []pipeline.Candidate{{Model: "m", LLM: fakeAnalysisLLM{complete: fakeArticleStudyJSON}}},
	}
	articles := newFakeNewsArticleStore(newsarticle.Article{ID: "a1", Source: "BBC", Title: "Headline", URL: "https://example.com/a", Status: newsarticle.StatusPending})
	audio := &ArticleAudio{Client: &fakeSpeaker{failWith: fmt.Errorf("tts server unreachable")}, Cache: &fakeCache{}}

	if err := RunArticleStudyInline(context.Background(), pipe, articles, audio, "a1", "BBC", "Headline", "snippet"); err != nil {
		t.Fatalf("RunArticleStudyInline() error = %v, want a TTS failure to be swallowed", err)
	}
	if got := articles.status("a1"); got != newsarticle.StatusDone {
		t.Fatalf("status = %q, want %q", got, newsarticle.StatusDone)
	}
}

// TestArticleAudioGenerateCachesUnderArticleAudioKey guards the exported
// entry point httpserver's audio handler calls directly on a cache miss —
// same generate-then-cache contract as the job's own best-effort path, but
// propagating the error instead of swallowing it.
func TestArticleAudioGenerateCachesUnderArticleAudioKey(t *testing.T) {
	speaker := &fakeSpeaker{}
	cache := &fakeCache{}
	audio := &ArticleAudio{Client: speaker, Cache: cache}

	got, err := audio.Generate(context.Background(), ArticleAudioKey("a2"), "hello there")
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}
	if string(got) != "audio-for:hello there" {
		t.Fatalf("Generate() = %q, want %q", got, "audio-for:hello there")
	}
	if cache.putCount() != 1 {
		t.Fatalf("cache Put called %d times, want 1", cache.putCount())
	}
}

// TestArticleAudioGeneratePropagatesSpeakerError guards that a real
// generation failure surfaces to the caller (httpserver's audio handler
// turns this into a 500) instead of silently caching nothing.
func TestArticleAudioGeneratePropagatesSpeakerError(t *testing.T) {
	audio := &ArticleAudio{Client: &fakeSpeaker{failWith: errors.New("boom")}, Cache: &fakeCache{}}

	if _, err := audio.Generate(context.Background(), "k", "text"); err == nil {
		t.Fatal("Generate() error = nil, want the Speak() failure to propagate")
	}
}

// TestRunArticleStudyPropagatesErrorAndMarksFailed guards the retry
// contract: an infrastructure failure marks the article StatusFailed for
// observability but propagates the error so asyncjob's reaper retries it.
func TestRunArticleStudyPropagatesErrorAndMarksFailed(t *testing.T) {
	pipe := &pipeline.Pipeline{
		Analysis: []pipeline.Candidate{{Model: "m", LLM: failingAnalysisLLM{}}},
	}
	articles := newFakeNewsArticleStore(newsarticle.Article{ID: "a1", Source: "BBC", Title: "Headline", URL: "https://example.com/a", Status: newsarticle.StatusPending})

	if err := RunArticleStudyInline(context.Background(), pipe, articles, nil, "a1", "BBC", "Headline", "snippet"); err == nil {
		t.Fatal("expected an error when the LLM call fails")
	}
	if got := articles.status("a1"); got != newsarticle.StatusFailed {
		t.Fatalf("status = %q, want %q", got, newsarticle.StatusFailed)
	}
}

// TestRunArticleStudyIsNoopForAlreadyDoneArticle guards against a stale
// reap-retry (or a racing concurrent draw's own job) re-generating an
// article that already completed.
func TestRunArticleStudyIsNoopForAlreadyDoneArticle(t *testing.T) {
	calls := 0
	pipe := &pipeline.Pipeline{
		Analysis: []pipeline.Candidate{{Model: "m", LLM: countingLLM{&calls, fakeArticleStudyJSON}}},
	}
	articles := newFakeNewsArticleStore(newsarticle.Article{
		ID: "a1", Status: newsarticle.StatusDone, Summary: "already there",
		SubQuestions: []newsarticle.SubQuestion{
			{Prompt: "p1", Options: []string{"a", "b"}, CorrectOptionIndex: 0, Explanation: "e"},
			{Prompt: "p2", Options: []string{"c", "d"}, CorrectOptionIndex: 0, Explanation: "e"},
		},
	})

	if err := RunArticleStudyInline(context.Background(), pipe, articles, nil, "a1", "BBC", "Headline", "snippet"); err != nil {
		t.Fatalf("RunArticleStudyInline() error = %v", err)
	}
	if calls != 0 {
		t.Fatalf("expected no LLM call for an already-done article, got %d calls", calls)
	}
}

// TestRunArticleStudyIsNoopForMissingArticle guards against a background job
// racing a state that no longer references the article it was handed.
func TestRunArticleStudyIsNoopForMissingArticle(t *testing.T) {
	pipe := &pipeline.Pipeline{}
	articles := newFakeNewsArticleStore()

	if err := RunArticleStudyInline(context.Background(), pipe, articles, nil, "does-not-exist", "BBC", "Headline", "snippet"); err != nil {
		t.Fatalf("RunArticleStudyInline() error = %v, want nil for a missing article", err)
	}
}

// TestSweepStaleArticleStudiesResumesAbandonedGeneration guards the primary
// orphan-recovery flow: a StatusPending article the fake reports stale (see
// markStale) gets claimed and regenerated, ending up StatusDone — this is
// what makes a redeploy- or crash-abandoned "오늘의 아티클" draw resume
// automatically without the learner doing anything, even with queue == nil
// (Redis not configured).
func TestSweepStaleArticleStudiesResumesAbandonedGeneration(t *testing.T) {
	pipe := &pipeline.Pipeline{
		Analysis: []pipeline.Candidate{{Model: "m", LLM: fakeAnalysisLLM{complete: fakeArticleStudyJSON}}},
	}
	articles := newFakeNewsArticleStore(newsarticle.Article{
		ID: "a-stale", Source: "BBC", Title: "Headline", URL: "https://example.com/stale", Description: "snippet", Status: newsarticle.StatusPending,
	})
	articles.markStale("a-stale")

	if err := SweepStaleArticleStudies(context.Background(), nil, pipe, articles, nil); err != nil {
		t.Fatalf("SweepStaleArticleStudies() error = %v", err)
	}
	// queue is nil, so EnqueueOrRunInline resumes it on a detached goroutine
	// — same "outlives the caller" shape as a real redeploy recovery.
	waitForCondition(t, 2*time.Second, func() bool {
		return articles.status("a-stale") == newsarticle.StatusDone
	})
	if got := articles.status("a-stale"); got != newsarticle.StatusDone {
		t.Fatalf("status after sweep = %q, want %q", got, newsarticle.StatusDone)
	}
}

// TestSweepStaleArticleStudiesSkipsNonStalePending guards against the sweep
// duplicating work for a generation that's still legitimately in progress —
// StalePending (faked here via markStale) is the only signal it trusts.
func TestSweepStaleArticleStudiesSkipsNonStalePending(t *testing.T) {
	calls := 0
	pipe := &pipeline.Pipeline{
		Analysis: []pipeline.Candidate{{Model: "m", LLM: countingLLM{&calls, fakeArticleStudyJSON}}},
	}
	articles := newFakeNewsArticleStore(newsarticle.Article{
		ID: "a-fresh", Source: "BBC", Title: "Headline", URL: "https://example.com/fresh", Status: newsarticle.StatusPending,
	})

	if err := SweepStaleArticleStudies(context.Background(), nil, pipe, articles, nil); err != nil {
		t.Fatalf("SweepStaleArticleStudies() error = %v", err)
	}
	if calls != 0 {
		t.Fatalf("expected no LLM call for a non-stale pending article, got %d calls", calls)
	}
}

func TestArticleStudyJobHandlerBadPayload(t *testing.T) {
	handler := ArticleStudyJobHandler(&pipeline.Pipeline{}, newFakeNewsArticleStore(), nil)
	job := asyncjob.Job{Kind: asyncjob.KindArticleStudy, Payload: json.RawMessage(`not json`)}
	if err := handler(context.Background(), job); err == nil {
		t.Fatalf("handler(bad payload) error = nil, want an unmarshal error")
	}
}

// TestEnqueueArticleStudyJobRunsInBackgroundAndPersists mirrors
// TestEnqueueWordVerifyJobRunsInBackgroundAndPersists against real Redis.
func TestEnqueueArticleStudyJobRunsInBackgroundAndPersists(t *testing.T) {
	rdb := requireReplyRedis(t)
	queue := asyncjob.NewQueue(rdb)
	pipe := &pipeline.Pipeline{
		Analysis: []pipeline.Candidate{{Model: "m", LLM: fakeAnalysisLLM{complete: fakeArticleStudyJSON}}},
	}
	articles := newFakeNewsArticleStore(newsarticle.Article{ID: "a-enqueue", Source: "BBC", Title: "Headline", URL: "https://example.com/enqueue", Status: newsarticle.StatusPending})

	if err := EnqueueArticleStudyJob(context.Background(), queue, pipe, articles, nil, "a-enqueue", "BBC", "Headline", "snippet"); err != nil {
		t.Fatalf("EnqueueArticleStudyJob() error = %v", err)
	}

	waitForCondition(t, 2*time.Second, func() bool {
		return articles.status("a-enqueue") == newsarticle.StatusDone
	})
	if got := articles.status("a-enqueue"); got != newsarticle.StatusDone {
		t.Fatalf("status = %q, want %q", got, newsarticle.StatusDone)
	}
}
