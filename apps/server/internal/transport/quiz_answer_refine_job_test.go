package transport

import (
	"context"
	"errors"
	"testing"
	"time"

	"buddy/server/internal/asyncjob"
	"buddy/server/internal/llm"
	"buddy/server/internal/pipeline"
	"buddy/server/internal/quizcache"
	"buddy/server/internal/wordreview"
	"buddy/server/internal/workguard"
)

// countingAnalysisLLM answers the quiz-check refinement contract
// ({"correct": bool}) while counting calls, so tests can prove the paid
// cascade ran — or was skipped because a verdict was already cached.
type countingAnalysisLLM struct {
	verdict string
	calls   int
	onCall  func()
}

func (f *countingAnalysisLLM) ChatStream(context.Context, string, []llm.Message, func(string)) (string, error) {
	return "", errFakeLLMUnavailable
}

func (f *countingAnalysisLLM) Complete(context.Context, string, []llm.Message, bool) (string, error) {
	f.calls++
	if f.onCall != nil {
		f.onCall()
	}
	return f.verdict, nil
}

func quizRefinePayload() QuizAnswerRefinePayload {
	return QuizAnswerRefinePayload{
		UserID:        "alex",
		SessionID:     "s1",
		Prompt:        "He ___ home late again.",
		Answer:        "sneaked",
		LearnerAnswer: "sneak",
		ChatDraft:     `{"correct":true}`,
	}
}

func TestRunQuizAnswerRefineStoresRefinedVerdict(t *testing.T) {
	rdb := requireReplyRedis(t)
	ctx := context.Background()
	cache := quizcache.New(rdb, time.Hour)
	analysis := &countingAnalysisLLM{verdict: `{"correct":false}`}
	pipe := &pipeline.Pipeline{Analysis: []pipeline.Candidate{{Model: "a", LLM: analysis}}}
	st := &guardedSessionStore{newFakeStore(), &deletedOwner{}}
	words := &guardedWordStore{newFakeWordReviewStore(wordreview.Word{ID: "w1", UserID: "alex"}), &deletedOwner{}}
	payload := quizRefinePayload()
	t.Cleanup(func() {
		_ = rdb.Del(context.Background(), quizcache.Key(payload.Prompt, payload.Answer, payload.LearnerAnswer)).Err()
	})

	// The Chat draft said true; Analysis overrides it, and the terminal
	// (not the draft) verdict is what gets cached for the next check.
	if err := runQuizAnswerRefine(ctx, pipe, cache, st, words, payload); err != nil {
		t.Fatalf("runQuizAnswerRefine: %v", err)
	}
	got, found := cache.Lookup(ctx, quizcache.Key(payload.Prompt, payload.Answer, payload.LearnerAnswer))
	if !found || got.Correct || got.Similar {
		t.Fatalf("cached verdict = %+v found=%v; want refined false", got, found)
	}
	if analysis.calls != 1 {
		t.Fatalf("analysis calls = %d, want 1", analysis.calls)
	}
}

func TestRunQuizAnswerRefineSkipsAlreadyCachedVerdict(t *testing.T) {
	rdb := requireReplyRedis(t)
	ctx := context.Background()
	cache := quizcache.New(rdb, time.Hour)
	payload := quizRefinePayload()
	key := quizcache.Key(payload.Prompt, payload.Answer, payload.LearnerAnswer)
	if err := cache.Save(ctx, key, quizcache.Verdict{Correct: true, Similar: true}); err != nil {
		t.Fatalf("seed Save: %v", err)
	}
	t.Cleanup(func() { _ = rdb.Del(context.Background(), key).Err() })

	analysis := &countingAnalysisLLM{verdict: `{"correct":false}`}
	pipe := &pipeline.Pipeline{Analysis: []pipeline.Candidate{{Model: "a", LLM: analysis}}}
	st := &guardedSessionStore{newFakeStore(), &deletedOwner{}}

	if err := runQuizAnswerRefine(ctx, pipe, cache, st, nil, payload); err != nil {
		t.Fatalf("runQuizAnswerRefine: %v", err)
	}
	if analysis.calls != 0 {
		t.Fatal("refinement re-ran the cascade for an already-cached verdict")
	}
}

func TestRunQuizAnswerRefineDropsDeletedOwner(t *testing.T) {
	rdb := requireReplyRedis(t)
	ctx := context.Background()
	cache := quizcache.New(rdb, time.Hour)
	for _, before := range []bool{true, false} {
		t.Run(map[bool]string{true: "before", false: "during"}[before], func(t *testing.T) {
			owner := &deletedOwner{}
			owner.deleted.Store(before)
			analysis := &countingAnalysisLLM{
				verdict: `{"correct":false}`,
				onCall:  func() { owner.deleted.Store(true) },
			}
			pipe := &pipeline.Pipeline{Analysis: []pipeline.Candidate{{Model: "a", LLM: analysis}}}
			st := &guardedSessionStore{newFakeStore(), owner}
			payload := quizRefinePayload()
			payload.Prompt += " " + map[bool]string{true: "before", false: "during"}[before]
			payload.SessionID = "del-" + map[bool]string{true: "before", false: "during"}[before]
			key := quizcache.Key(payload.Prompt, payload.Answer, payload.LearnerAnswer)
			t.Cleanup(func() { _ = rdb.Del(context.Background(), key).Err() })

			err := runQuizAnswerRefine(ctx, pipe, cache, st, nil, payload)
			if !errors.Is(err, workguard.ErrDeleted) {
				t.Fatalf("error = %v; want workguard.ErrDeleted", err)
			}
			wantCalls := 0
			if !before {
				wantCalls = 1
			}
			if analysis.calls != wantCalls {
				t.Fatalf("analysis calls = %d, want %d", analysis.calls, wantCalls)
			}
			if _, found := cache.Lookup(ctx, key); found {
				t.Fatal("deleted owner stored a verdict")
			}
		})
	}
}

func TestRunQuizAnswerRefineRejectsIncompletePayload(t *testing.T) {
	cache := &fakeQuizVerdictStore{}
	pipe := &pipeline.Pipeline{}
	for name, mutate := range map[string]func(*QuizAnswerRefinePayload){
		"no prompt":  func(p *QuizAnswerRefinePayload) { p.Prompt = "  " },
		"no answer":  func(p *QuizAnswerRefinePayload) { p.Answer = "" },
		"no learner": func(p *QuizAnswerRefinePayload) { p.LearnerAnswer = "" },
		"no draft":   func(p *QuizAnswerRefinePayload) { p.ChatDraft = "" },
	} {
		payload := quizRefinePayload()
		mutate(&payload)
		if err := runQuizAnswerRefine(context.Background(), pipe, cache, nil, nil, payload); err == nil {
			t.Fatalf("%s: expected a payload error", name)
		}
	}
	if len(cache.saved) != 0 {
		t.Fatalf("rejected payloads still stored %v", cache.saved)
	}
}

// fakeQuizVerdictStore is an in-memory quizcache.Store for cases where a
// real Redis would add nothing (payload validation runs before any Redis
// touch) and for asserting nothing was written on failure paths.
type fakeQuizVerdictStore struct{ saved []string }

func (f *fakeQuizVerdictStore) Lookup(context.Context, string) (quizcache.Verdict, bool) {
	return quizcache.Verdict{}, false
}
func (f *fakeQuizVerdictStore) Save(_ context.Context, key string, _ quizcache.Verdict) error {
	f.saved = append(f.saved, key)
	return nil
}
func (f *fakeQuizVerdictStore) Renew(context.Context, string) {}

func TestEnqueueQuizAnswerRefineRunsInBackgroundAndCaches(t *testing.T) {
	rdb := requireReplyRedis(t)
	queue := asyncjob.NewQueue(rdb)
	cache := quizcache.New(rdb, time.Hour)
	analysis := &countingAnalysisLLM{verdict: `{"correct":false}`}
	pipe := &pipeline.Pipeline{Analysis: []pipeline.Candidate{{Model: "a", LLM: analysis}}}
	st := &guardedSessionStore{newFakeStore(), &deletedOwner{}}
	payload := quizRefinePayload()
	payload.UserID = "enqueue-user"
	payload.SessionID = "enqueue-session"
	key := quizcache.Key(payload.Prompt, payload.Answer, payload.LearnerAnswer)
	t.Cleanup(func() { _ = rdb.Del(context.Background(), key).Err() })

	if err := EnqueueQuizAnswerRefine(context.Background(), queue, pipe, cache, st, nil, payload); err != nil {
		t.Fatalf("EnqueueQuizAnswerRefine: %v", err)
	}
	waitForCondition(t, 5*time.Second, func() bool {
		_, found := cache.Lookup(context.Background(), key)
		return found
	})
	got, found := cache.Lookup(context.Background(), key)
	if !found || got.Correct {
		t.Fatalf("cached verdict = %+v found=%v; want refined false", got, found)
	}
}

func TestEnqueueQuizAnswerRefineWithoutQueueIsNoop(t *testing.T) {
	payload := quizRefinePayload()
	if err := EnqueueQuizAnswerRefine(context.Background(), nil, &pipeline.Pipeline{}, nil, nil, nil, payload); err != nil {
		t.Fatalf("nil queue must be a no-op, got %v", err)
	}
}
