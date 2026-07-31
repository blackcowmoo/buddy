package transport

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"buddy/server/internal/asyncjob"
	"buddy/server/internal/pipeline"
	"buddy/server/internal/wordreview"
)

// fakeWordReviewStore is a minimal in-memory wordreview.Store for these job
// tests — real SQL behavior is covered by internal/wordreview's own tests.
// Guarded by mu since EnqueueWordVerifyJob's background path (and this
// package's own concurrent job execution) can touch it from more than one
// goroutine.
type fakeWordReviewStore struct {
	mu    sync.Mutex
	words map[string]wordreview.Word // by id
}

func newFakeWordReviewStore(words ...wordreview.Word) *fakeWordReviewStore {
	s := &fakeWordReviewStore{words: map[string]wordreview.Word{}}
	for _, w := range words {
		s.words[w.ID] = w
	}
	return s
}

func (s *fakeWordReviewStore) Save(ctx context.Context, userID, word, meaning, example string) (wordreview.Word, error) {
	return wordreview.Word{}, nil
}

func (s *fakeWordReviewStore) Get(ctx context.Context, userID, id string) (wordreview.Word, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	w, ok := s.words[id]
	if !ok || w.UserID != userID {
		return wordreview.Word{}, nil
	}
	return w, nil
}

func (s *fakeWordReviewStore) List(ctx context.Context, userID string) ([]wordreview.Word, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []wordreview.Word
	for _, w := range s.words {
		if w.UserID == userID {
			out = append(out, w)
		}
	}
	return out, nil
}

func (s *fakeWordReviewStore) DueCount(ctx context.Context, userID string, now time.Time) (int, error) {
	return 0, nil
}

func (s *fakeWordReviewStore) MarkVerified(ctx context.Context, userID, id string, now time.Time) (wordreview.Word, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	w, ok := s.words[id]
	if !ok || w.UserID != userID {
		return wordreview.Word{}, nil
	}
	w.Status = wordreview.StatusVerified
	s.words[id] = w
	return w, nil
}

func (s *fakeWordReviewStore) MarkRejected(ctx context.Context, userID, id string, reason string) (wordreview.Word, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	w, ok := s.words[id]
	if !ok || w.UserID != userID {
		return wordreview.Word{}, nil
	}
	w.Status = wordreview.StatusRejected
	w.VerifyReason = reason
	s.words[id] = w
	return w, nil
}

func (s *fakeWordReviewStore) Review(ctx context.Context, userID, id string, correct bool, now time.Time) (wordreview.Word, error) {
	return wordreview.Word{}, nil
}

func (s *fakeWordReviewStore) Delete(ctx context.Context, userID, id string) error { return nil }

func (s *fakeWordReviewStore) status(id string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.words[id].Status
}

func (s *fakeWordReviewStore) reason(id string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.words[id].VerifyReason
}

func (s *fakeWordReviewStore) Close() error { return nil }

// TestRunWordVerifyMarksVerifiedWhenAllJudgesAgree guards the primary flow:
// a Pending word whose judges unanimously agree valid ends up Verified.
func TestRunWordVerifyMarksVerifiedWhenAllJudgesAgree(t *testing.T) {
	pipe := &pipeline.Pipeline{
		Analysis: []pipeline.Candidate{{Model: "m", LLM: fakeAnalysisLLM{complete: `{"valid":true,"reason":""}`}}},
	}
	words := newFakeWordReviewStore(wordreview.Word{ID: "w1", UserID: "alex", Word: "furious", Meaning: "화가 난", Example: "She was furious.", Status: wordreview.StatusPending})

	if err := RunWordVerifyInline(context.Background(), pipe, words, "alex", "w1"); err != nil {
		t.Fatalf("RunWordVerifyInline() error = %v", err)
	}
	if got := words.status("w1"); got != wordreview.StatusVerified {
		t.Fatalf("status = %q, want %q", got, wordreview.StatusVerified)
	}
}

// TestRunWordVerifyMarksRejectedWithReason guards the reject path: a
// dissenting judge's reason is what gets recorded, never silently dropped.
func TestRunWordVerifyMarksRejectedWithReason(t *testing.T) {
	pipe := &pipeline.Pipeline{
		Analysis: []pipeline.Candidate{{Model: "m", LLM: fakeAnalysisLLM{complete: `{"valid":false,"reason":"not a real word"}`}}},
	}
	words := newFakeWordReviewStore(wordreview.Word{ID: "w1", UserID: "alex", Word: "xyzzy", Meaning: "존재하지 않는 단어", Example: "xyzzy the door.", Status: wordreview.StatusPending})

	if err := RunWordVerifyInline(context.Background(), pipe, words, "alex", "w1"); err != nil {
		t.Fatalf("RunWordVerifyInline() error = %v", err)
	}
	if got := words.status("w1"); got != wordreview.StatusRejected {
		t.Fatalf("status = %q, want %q", got, wordreview.StatusRejected)
	}
	if got := words.reason("w1"); got != "not a real word" {
		t.Fatalf("reason = %q, want the dissenting judge's reason", got)
	}
}

// TestRunWordVerifyPropagatesErrorAndLeavesWordPending guards the retry
// contract: an infrastructure failure (every judge unreachable) must
// propagate the error (so asyncjob's reaper retries) and must never mark the
// word Rejected over a transient failure.
func TestRunWordVerifyPropagatesErrorAndLeavesWordPending(t *testing.T) {
	pipe := &pipeline.Pipeline{
		Analysis: []pipeline.Candidate{{Model: "m", LLM: failingAnalysisLLM{}}},
	}
	words := newFakeWordReviewStore(wordreview.Word{ID: "w1", UserID: "alex", Word: "furious", Meaning: "화가 난", Example: "She was furious.", Status: wordreview.StatusPending})

	if err := RunWordVerifyInline(context.Background(), pipe, words, "alex", "w1"); err == nil {
		t.Fatal("expected an error when every judge call fails")
	}
	if got := words.status("w1"); got != wordreview.StatusPending {
		t.Fatalf("status = %q, want unchanged %q after an infra failure", got, wordreview.StatusPending)
	}
}

// TestRunWordVerifyIsNoopForAlreadyDecidedWord guards against a stale
// reap-retry re-verifying (and potentially flip-flopping) a word that
// already reached a terminal status via an earlier attempt.
func TestRunWordVerifyIsNoopForAlreadyDecidedWord(t *testing.T) {
	calls := 0
	pipe := &pipeline.Pipeline{
		Analysis: []pipeline.Candidate{{Model: "m", LLM: countingLLM{&calls, `{"valid":true,"reason":""}`}}},
	}
	words := newFakeWordReviewStore(wordreview.Word{ID: "w1", UserID: "alex", Word: "furious", Status: wordreview.StatusVerified})

	if err := RunWordVerifyInline(context.Background(), pipe, words, "alex", "w1"); err != nil {
		t.Fatalf("RunWordVerifyInline() error = %v", err)
	}
	if calls != 0 {
		t.Fatalf("expected no LLM call for an already-verified word, got %d calls", calls)
	}
}

// TestRunWordVerifyIsNoopForDeletedWord guards against a background job
// racing a learner deleting the word mid-verification.
func TestRunWordVerifyIsNoopForDeletedWord(t *testing.T) {
	pipe := &pipeline.Pipeline{}
	words := newFakeWordReviewStore()

	if err := RunWordVerifyInline(context.Background(), pipe, words, "alex", "does-not-exist"); err != nil {
		t.Fatalf("RunWordVerifyInline() error = %v, want nil for an already-deleted word", err)
	}
}

func TestWordVerifyJobHandlerBadPayload(t *testing.T) {
	handler := WordVerifyJobHandler(&pipeline.Pipeline{}, newFakeWordReviewStore())
	job := asyncjob.Job{Kind: asyncjob.KindWordVerify, Payload: json.RawMessage(`not json`)}
	if err := handler(context.Background(), job); err == nil {
		t.Fatalf("handler(bad payload) error = nil, want an unmarshal error")
	}
}

// TestEnqueueWordVerifyJobRunsInBackgroundAndPersists mirrors
// TestEnqueueStudyQuizJobRunsInBackgroundAndPersists against real Redis.
func TestEnqueueWordVerifyJobRunsInBackgroundAndPersists(t *testing.T) {
	rdb := requireReplyRedis(t)
	queue := asyncjob.NewQueue(rdb)
	pipe := &pipeline.Pipeline{
		Analysis: []pipeline.Candidate{{Model: "m", LLM: fakeAnalysisLLM{complete: `{"valid":true,"reason":""}`}}},
	}
	words := newFakeWordReviewStore(wordreview.Word{ID: "w-enqueue", UserID: "alex", Word: "furious", Status: wordreview.StatusPending})

	if err := EnqueueWordVerifyJob(context.Background(), queue, pipe, words, "alex", "w-enqueue"); err != nil {
		t.Fatalf("EnqueueWordVerifyJob() error = %v", err)
	}

	waitForCondition(t, 2*time.Second, func() bool {
		return words.status("w-enqueue") == wordreview.StatusVerified
	})
	if got := words.status("w-enqueue"); got != wordreview.StatusVerified {
		t.Fatalf("status = %q, want %q", got, wordreview.StatusVerified)
	}
}
