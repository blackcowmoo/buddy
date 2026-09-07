package transport

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"buddy/server/internal/asyncjob"
	"buddy/server/internal/llm"
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

// Save mirrors wordreview.MySQLStore.Save's dedup semantics: re-saving the
// same (userID, word, meaning) triple returns the existing row untouched
// rather than creating a duplicate — captureCorrectionWords relies on this
// to stay idempotent across the fast/ensemble and queued/live correction
// paths that can each try to save the same captured word.
func (s *fakeWordReviewStore) Save(ctx context.Context, userID, word, meaning, example string) (wordreview.Word, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, w := range s.words {
		if w.UserID == userID && w.Word == word && w.Meaning == meaning {
			return w, nil
		}
	}
	now := time.Now()
	w := wordreview.Word{
		ID:           uuid.New().String(),
		UserID:       userID,
		Word:         word,
		Meaning:      meaning,
		Example:      example,
		NextReviewAt: now,
		CreatedAt:    now,
		Status:       wordreview.StatusPending,
	}
	s.words[w.ID] = w
	return w, nil
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

func (s *fakeWordReviewStore) Review(ctx context.Context, userID, id string, correct, repeat bool, now time.Time) (wordreview.Word, error) {
	return wordreview.Word{}, nil
}

func (s *fakeWordReviewStore) Delete(ctx context.Context, userID, id string) error { return nil }

func (s *fakeWordReviewStore) SaveQuestion(ctx context.Context, userID, id string, question wordreview.Question) (wordreview.Word, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	w, ok := s.words[id]
	if !ok || w.UserID != userID {
		return wordreview.Word{}, false, nil
	}
	if w.ReviewQuestion.Version > question.Version ||
		(w.ReviewQuestion.Version == question.Version && w.ReviewQuestion.Prompt != "" && w.ReviewQuestion.Answer != "") {
		return w, false, nil
	}
	w.ReviewQuestion = question
	s.words[id] = w
	return w, true, nil
}

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

func (s *fakeWordReviewStore) question(id string) wordreview.Question {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.words[id].ReviewQuestion
}

func (s *fakeWordReviewStore) Close() error { return nil }

// wordLearningLLM returns the right strict-JSON contract for both steps in a
// word verification job. Each pipeline stage receives the authoritative task
// in its system message, so the same double can model Chat, Analysis, and
// Judge without relying on call timing.
type wordLearningLLM struct {
	valid  bool
	reason string
}

func (f wordLearningLLM) ChatStream(context.Context, string, []llm.Message, func(string)) (string, error) {
	return "", errFakeLLMUnavailable
}

func (f wordLearningLLM) Complete(_ context.Context, _ string, msgs []llm.Message, _ bool) (string, error) {
	if strings.Contains(msgs[0].Content, "fill-in-the-blank recall question") {
		return `{"prompt":"She was ___.","answer":"furious"}`, nil
	}
	return fmt.Sprintf(`{"valid":%t,"reason":%q}`, f.valid, f.reason), nil
}

// TestRunWordVerifyMarksVerifiedWhenAllJudgesAgree guards the primary flow:
// a Pending word whose judges unanimously agree valid ends up Verified.
func TestRunWordVerifyMarksVerifiedWhenAllJudgesAgree(t *testing.T) {
	model := wordLearningLLM{valid: true}
	pipe := &pipeline.Pipeline{
		LLM:        model,
		ChatModel:  "chat",
		Analysis:   []pipeline.Candidate{{Model: "analysis", LLM: model}},
		Judge:      model,
		JudgeModel: "judge",
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
	words.words["w1"] = wordreview.Word{
		ID: "w1", UserID: "alex", Word: "furious", Status: wordreview.StatusVerified,
		ReviewQuestion: wordreview.Question{Version: wordreview.CurrentQuestionVersion, Prompt: "She was ___.", Answer: "furious"},
	}

	if err := RunWordVerifyInline(context.Background(), pipe, words, "alex", "w1"); err != nil {
		t.Fatalf("RunWordVerifyInline() error = %v", err)
	}
	if calls != 0 {
		t.Fatalf("expected no LLM call for an already-verified word, got %d calls", calls)
	}
}

func TestRunWordVerifyBackfillsVersionedQuestionWithSentenceForm(t *testing.T) {
	pipe := &pipeline.Pipeline{
		LLM:       fakeAnalysisLLM{complete: `{"prompt":"They ___ the files yesterday.","answer":"organized"}`},
		ChatModel: "m",
	}
	words := newFakeWordReviewStore(wordreview.Word{
		ID: "w-old", UserID: "alex", Word: "organize", Meaning: "정리하다",
		Example: "They organized the files.", Status: wordreview.StatusVerified,
	})

	if err := RunWordVerifyInline(context.Background(), pipe, words, "alex", "w-old"); err != nil {
		t.Fatalf("RunWordVerifyInline() error = %v", err)
	}
	got := words.question("w-old")
	if got.Version != wordreview.CurrentQuestionVersion || got.Answer != "organized" || got.Prompt != "They ___ the files yesterday." {
		t.Fatalf("question = %+v, want current version with the sentence-required past form", got)
	}
}

func TestWordVerifyJobHandlerIgnoresLegacyQuestionGenerationPayload(t *testing.T) {
	calls := 0
	pipe := &pipeline.Pipeline{LLM: countingLLM{&calls, `{"prompt":"She was ___.","answer":"furious"}`}, ChatModel: "m"}
	words := newFakeWordReviewStore(wordreview.Word{ID: "w1", UserID: "alex", Word: "furious", Status: wordreview.StatusVerified})
	handler := WordVerifyJobHandler(pipe, words)
	payload, err := json.Marshal(wordVerifyJobPayload{UserID: "alex", WordID: "w1"}) // legacy: no QuestionVersion
	if err != nil {
		t.Fatal(err)
	}
	if err := handler(context.Background(), asyncjob.Job{Kind: asyncjob.KindWordVerify, Payload: payload}); err != nil {
		t.Fatalf("handler() error = %v", err)
	}
	if calls != 0 || words.question("w1").Version != 0 {
		t.Fatalf("legacy payload generated a question: calls=%d question=%+v", calls, words.question("w1"))
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
	model := wordLearningLLM{valid: true}
	pipe := &pipeline.Pipeline{
		LLM:        model,
		ChatModel:  "chat",
		Analysis:   []pipeline.Candidate{{Model: "analysis", LLM: model}},
		Judge:      model,
		JudgeModel: "judge",
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
