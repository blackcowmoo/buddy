package transport

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"buddy/server/internal/asyncjob"
	"buddy/server/internal/newsarticle"
	"buddy/server/internal/pipeline"
)

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

func (s *fakeNewsArticleStore) ReserveArticle(ctx context.Context, source, title, url, description string) (newsarticle.Article, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.articles {
		if a.URL == url {
			return a, nil
		}
	}
	a := newsarticle.Article{ID: uuid.New().String(), Source: source, Title: title, URL: url, Description: description, Status: newsarticle.StatusPending, CreatedAt: time.Now()}
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

func (s *fakeNewsArticleStore) CompleteArticle(ctx context.Context, id, summary string, choices []string, correctIndex int, explanation string) (newsarticle.Article, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.articles[id]
	if !ok || a.Status != newsarticle.StatusPending {
		return a, nil
	}
	a.Summary = summary
	a.Choices = choices
	a.CorrectIndex = correctIndex
	a.Explanation = explanation
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
func (s *fakeNewsArticleStore) Answer(ctx context.Context, userID, id string, selectedIndex int) (newsarticle.Instance, error) {
	return newsarticle.Instance{}, nil
}
func (s *fakeNewsArticleStore) Delete(ctx context.Context, userID, id string) error { return nil }
func (s *fakeNewsArticleStore) Close() error                                        { return nil }

func (s *fakeNewsArticleStore) status(id string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.articles[id].Status
}

const fakeArticleStudyJSON = `{"summary":"A short English study paragraph.","choices":["정확한 해석","틀린 해석 1","틀린 해석 2","틀린 해석 3"],"correctIndex":0,"explanation":"정확한 해석이 원문의 의미를 담고 있기 때문입니다."}`

// TestRunArticleStudyCompletesAPendingArticle guards the primary flow: a
// StatusPending article whose LLM call succeeds ends up StatusDone with the
// generated content persisted.
func TestRunArticleStudyCompletesAPendingArticle(t *testing.T) {
	pipe := &pipeline.Pipeline{
		Analysis: []pipeline.Candidate{{Model: "m", LLM: fakeAnalysisLLM{complete: fakeArticleStudyJSON}}},
	}
	articles := newFakeNewsArticleStore(newsarticle.Article{ID: "a1", Source: "BBC", Title: "Headline", URL: "https://example.com/a", Status: newsarticle.StatusPending})

	if err := RunArticleStudyInline(context.Background(), pipe, articles, "a1", "BBC", "Headline", "snippet"); err != nil {
		t.Fatalf("RunArticleStudyInline() error = %v", err)
	}
	if got := articles.status("a1"); got != newsarticle.StatusDone {
		t.Fatalf("status = %q, want %q", got, newsarticle.StatusDone)
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

	if err := RunArticleStudyInline(context.Background(), pipe, articles, "a1", "BBC", "Headline", "snippet"); err == nil {
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
		ID: "a1", Status: newsarticle.StatusDone, Summary: "already there", Choices: []string{"a", "b", "c", "d"},
	})

	if err := RunArticleStudyInline(context.Background(), pipe, articles, "a1", "BBC", "Headline", "snippet"); err != nil {
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

	if err := RunArticleStudyInline(context.Background(), pipe, articles, "does-not-exist", "BBC", "Headline", "snippet"); err != nil {
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

	if err := SweepStaleArticleStudies(context.Background(), nil, pipe, articles); err != nil {
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

	if err := SweepStaleArticleStudies(context.Background(), nil, pipe, articles); err != nil {
		t.Fatalf("SweepStaleArticleStudies() error = %v", err)
	}
	if calls != 0 {
		t.Fatalf("expected no LLM call for a non-stale pending article, got %d calls", calls)
	}
}

func TestArticleStudyJobHandlerBadPayload(t *testing.T) {
	handler := ArticleStudyJobHandler(&pipeline.Pipeline{}, newFakeNewsArticleStore())
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

	if err := EnqueueArticleStudyJob(context.Background(), queue, pipe, articles, "a-enqueue", "BBC", "Headline", "snippet"); err != nil {
		t.Fatalf("EnqueueArticleStudyJob() error = %v", err)
	}

	waitForCondition(t, 2*time.Second, func() bool {
		return articles.status("a-enqueue") == newsarticle.StatusDone
	})
	if got := articles.status("a-enqueue"); got != newsarticle.StatusDone {
		t.Fatalf("status = %q, want %q", got, newsarticle.StatusDone)
	}
}
