package transport

import (
	"context"
	"errors"
	"fmt"
	"time"

	"buddy/server/internal/asyncjob"
	"buddy/server/internal/pipeline"
	"buddy/server/internal/wordreview"
	"buddy/server/internal/workguard"
)

const (
	WordResearchClaimTTL          = 25 * time.Hour
	WordResearchWorkerConcurrency = 4
)

type wordResearchJobPayload struct {
	UserID, WordID  string
	CleanupMeanings bool
	Revision        int
}

func WordResearchJobHandler(pipe *pipeline.Pipeline, words wordreview.Store) asyncjob.Handler {
	return func(ctx context.Context, job asyncjob.Job) error {
		return asyncjob.DecodePayloadHandler(asyncjob.KindWordResearch, func(ctx context.Context, p wordResearchJobPayload) error {
			if p.CleanupMeanings {
				return runWordMeaningCleanup(ctx, pipe, words, p.UserID)
			}
			return runWordResearch(ctx, pipe, words, p.UserID, p.WordID, p.Revision, job.Attempts+1 >= asyncjob.MaxAttempts)
		})(ctx, job)
	}
}

// RunWordResearchInline shares the queued research path without serializing an
// internal payload. Callers detach it from the HTTP request before running it.
func RunWordResearchInline(ctx context.Context, pipe *pipeline.Pipeline, words wordreview.Store, userID, wordID string, revision int) error {
	return runWordResearch(ctx, pipe, words, userID, wordID, revision, true)
}

func runWordResearch(ctx context.Context, pipe *pipeline.Pipeline, words wordreview.Store, userID, wordID string, revision int, finalAttempt bool) error {
	ctx = workguard.BindStore(ctx, words, userID, wordID)
	if err := workguard.Check(ctx); err != nil {
		return err
	}
	store, ok := words.(wordreview.ResearchStore)
	if !ok {
		return fmt.Errorf("word research: store does not support research")
	}
	target, err := words.Get(ctx, userID, wordID)
	if err != nil {
		return err
	}
	if target.ID == "" || target.ResearchStatus != wordreview.ResearchPending {
		return nil
	}
	// Revision zero accepts legacy queued jobs from before revision tracking.
	if revision > 0 && target.ResearchRevision != revision {
		return nil
	}
	word := target.OriginalWord
	if word == "" {
		word = target.Word
	}
	results, err := pipe.DefineWordMeanings(ctx, word, target.Meaning, target.Example, target.VerifyReason)
	if err != nil {
		// Semantic exhaustion is a completed search with no safe candidates,
		// not an infrastructure error for the durable queue to repeat forever.
		if errors.Is(err, pipeline.ErrNoVerifiedWordMeanings) {
			_, saveErr := store.FinishResearch(ctx, target, nil)
			return saveErr
		}
		if finalAttempt && workguard.Check(ctx) == nil && ctx.Err() == nil {
			_, saveErr := store.FinishResearch(ctx, target, nil)
			return errors.Join(err, saveErr)
		}
		return err
	}
	converted := make([]wordreview.ResearchSuggestion, len(results))
	for i, result := range results {
		converted[i] = wordreview.ResearchSuggestion{Word: result.Word, Meaning: result.Meaning, Example: result.Example, Verified: true}
	}
	if err := workguard.Check(ctx); err != nil {
		return err
	}
	_, err = store.FinishResearch(ctx, target, converted)
	return err
}

func EnqueueWordResearchJob(ctx context.Context, queue *asyncjob.Queue, pipe *pipeline.Pipeline, words wordreview.Store, userID, wordID string, revision int) error {
	p := wordResearchJobPayload{UserID: userID, WordID: wordID, Revision: revision}
	// A new search can start after results are saved but before the old queue
	// claim is released. Dedupe per revision so that request is not lost.
	key := fmt.Sprintf("%s/%s:r%d", userID, wordID, revision)
	return queue.EnqueueAndRunInBackground(ctx, asyncjob.KindWordResearch, key, key, p, WordResearchClaimTTL, WordResearchJobHandler(pipe, words))
}
