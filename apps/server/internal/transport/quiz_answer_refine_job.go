package transport

import (
	"context"
	"fmt"
	"strings"
	"time"

	"buddy/server/internal/asyncjob"
	"buddy/server/internal/pipeline"
	"buddy/server/internal/quizcache"
	"buddy/server/internal/workguard"
)

// QuizAnswerRefineClaimTTL mirrors PhraseHintClaimTTL's reasoning (one
// cascade against a potentially slow local model). Concurrency stays low:
// this job only warms the verdict cache behind an already-answered question,
// so no learner is waiting on it and it must never crowd out generation
// work that someone is watching.
const (
	QuizAnswerRefineClaimTTL          = 25 * time.Hour
	QuizAnswerRefineWorkerConcurrency = 2
)

// QuizAnswerRefinePayload carries the exact Chat draft to refine (never
// regenerated — pipeline.CheckQuizAnswerFast's lossless handoff) plus the
// owner IDs the job rebinds workguard to, so refining stops when the session
// or word the answer came from is deleted.
type QuizAnswerRefinePayload struct {
	UserID            string
	SessionID         string
	WordID            string
	Prompt            string
	Answer            string
	AcceptableAnswers []string
	LearnerAnswer     string
	ChatDraft         string
}

func runQuizAnswerRefine(ctx context.Context, pipe *pipeline.Pipeline, cache quizcache.Store, sessions, words any, payload QuizAnswerRefinePayload) error {
	if pipe == nil || cache == nil {
		return fmt.Errorf("quiz-answer-refine: pipeline and verdict cache are required")
	}
	if strings.TrimSpace(payload.Prompt) == "" || strings.TrimSpace(payload.Answer) == "" ||
		strings.TrimSpace(payload.LearnerAnswer) == "" || strings.TrimSpace(payload.ChatDraft) == "" {
		return fmt.Errorf("quiz-answer-refine: incomplete payload")
	}
	if payload.SessionID != "" {
		ctx = workguard.BindStore(ctx, sessions, payload.UserID, payload.SessionID)
	}
	if payload.WordID != "" {
		ctx = workguard.BindStore(ctx, words, payload.UserID, payload.WordID)
	}
	if err := workguard.Check(ctx); err != nil {
		return err
	}
	key := quizcache.Key(payload.Prompt, payload.Answer, payload.LearnerAnswer)
	// A concurrent identical refinement (same triple from another learner or
	// replica, different dedupe identity) may already have stored the
	// verdict; skip the paid cascade.
	if _, found := cache.Lookup(ctx, key); found {
		return nil
	}
	refined, err := pipe.RefineQuizAnswerFromDraft(ctx, payload.Prompt, payload.Answer, payload.AcceptableAnswers, payload.LearnerAnswer, payload.ChatDraft)
	if err != nil {
		return fmt.Errorf("quiz-answer-refine: %w", err)
	}
	if err := cache.Save(ctx, key, quizcache.Verdict{Correct: refined, Similar: refined}); err != nil {
		return fmt.Errorf("quiz-answer-refine: store verdict: %w", err)
	}
	return nil
}

func QuizAnswerRefineJobHandler(pipe *pipeline.Pipeline, cache quizcache.Store, sessions, words any) asyncjob.Handler {
	return asyncjob.DecodePayloadHandler(asyncjob.KindQuizAnswerRefine, func(ctx context.Context, payload QuizAnswerRefinePayload) error {
		return runQuizAnswerRefine(ctx, pipe, cache, sessions, words, payload)
	})
}

// EnqueueQuizAnswerRefine durably queues the Analysis -> Judge refinement of
// a Chat verdict the learner already received. A nil queue (no Redis) is a
// silent no-op by design: without Redis there is no verdict cache, so the
// refinement has nothing to produce for.
func EnqueueQuizAnswerRefine(ctx context.Context, queue *asyncjob.Queue, pipe *pipeline.Pipeline, cache quizcache.Store, sessions, words any, payload QuizAnswerRefinePayload) error {
	if queue == nil {
		return nil
	}
	// Dedupe on owner + verdict key: identical checks collapse, while the
	// same triple entered under different owners stays separately guarded —
	// one owner's deletion must not strand another owner's warm-up.
	key := strings.Join([]string{payload.UserID, payload.SessionID, payload.WordID, quizcache.Key(payload.Prompt, payload.Answer, payload.LearnerAnswer)}, "\x00")
	return queue.EnqueueAndRunInBackground(ctx, asyncjob.KindQuizAnswerRefine, key, key, payload, QuizAnswerRefineClaimTTL, QuizAnswerRefineJobHandler(pipe, cache, sessions, words))
}
