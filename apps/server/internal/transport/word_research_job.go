package transport

import (
	"context"
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
}

func WordResearchJobHandler(pipe *pipeline.Pipeline, words wordreview.Store) asyncjob.Handler {
	return asyncjob.DecodePayloadHandler(asyncjob.KindWordResearch, func(ctx context.Context, p wordResearchJobPayload) error {
		if p.CleanupMeanings {
			return runWordMeaningCleanup(ctx, pipe, words, p.UserID)
		}
		return RunWordResearchInline(ctx, pipe, words, p.UserID, p.WordID)
	})
}

// RunWordResearchInline shares the queued research path without serializing an
// internal payload. Callers detach it from the HTTP request before running it.
func RunWordResearchInline(ctx context.Context, pipe *pipeline.Pipeline, words wordreview.Store, userID, wordID string) error {
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
	if target.ID == "" {
		return nil
	}
	word := target.OriginalWord
	if word == "" {
		word = target.Word
	}
	results, err := pipe.DefineWordMeanings(ctx, word, target.Example)
	if err != nil {
		return err
	}
	converted := make([]wordreview.ResearchSuggestion, len(results))
	for i, result := range results {
		converted[i] = wordreview.ResearchSuggestion{Word: result.Word, Meaning: result.Meaning, Example: result.Example}
	}
	_, err = store.FinishResearch(ctx, userID, wordID, converted)
	return err
}

func EnqueueWordResearchJob(ctx context.Context, queue *asyncjob.Queue, pipe *pipeline.Pipeline, words wordreview.Store, userID, wordID string) error {
	p := wordResearchJobPayload{UserID: userID, WordID: wordID}
	return queue.EnqueueAndRunInBackground(ctx, asyncjob.KindWordResearch, userID+"/"+wordID, userID+"/"+wordID, p, WordResearchClaimTTL, WordResearchJobHandler(pipe, words))
}
