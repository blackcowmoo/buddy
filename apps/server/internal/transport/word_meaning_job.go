package transport

import (
	"context"
	"errors"
	"fmt"
	"log"

	"buddy/server/internal/asyncjob"
	"buddy/server/internal/pipeline"
	"buddy/server/internal/wordreview"
	"buddy/server/internal/workguard"
)

// Reuse the vocabulary-research worker pool with a distinct payload mode and
// dedupe key. Each word commits separately, so a replica restart only repeats
// unfinished entries. Work remains sequential to avoid flooding local models.
func runWordMeaningCleanup(ctx context.Context, pipe *pipeline.Pipeline, words wordreview.Store, userID string) error {
	store, ok := words.(wordreview.MeaningStore)
	if !ok {
		return fmt.Errorf("word meanings: unsupported store")
	}
	list, err := store.PendingMeanings(ctx, userID)
	if err != nil {
		return err
	}
	for _, word := range list {
		if err := ctx.Err(); err != nil {
			return err
		}
		if word.Status != wordreview.StatusVerified || word.MeaningStatus != "pending" || word.MeaningVersion >= wordreview.CurrentMeaningVersion {
			continue
		}
		wordCtx := workguard.BindStore(ctx, words, userID, word.ID)
		meaning, err := pipe.NormalizeWordMeaning(wordCtx, word.Word, word.Meaning, word.Example)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if checkErr := workguard.Check(wordCtx); checkErr != nil {
			if errors.Is(checkErr, workguard.ErrDeleted) {
				continue
			}
			return checkErr
		}
		if err != nil {
			log.Printf("word meaning cleanup %s/%s: %v", userID, word.ID, err)
			if err := store.FailMeaning(wordCtx, word, "뜻을 확실하게 정리하지 못했어요. 기존 뜻을 유지했어요."); err != nil {
				return err
			}
			continue
		}
		if err := store.SaveMeaning(wordCtx, word, meaning); err != nil {
			return err
		}
	}
	return nil
}

func EnqueueWordMeaningCleanup(ctx context.Context, queue *asyncjob.Queue, pipe *pipeline.Pipeline, words wordreview.Store, userID string) error {
	payload := wordResearchJobPayload{UserID: userID, CleanupMeanings: true}
	key := fmt.Sprintf("%s:meanings:v%d", userID, wordreview.CurrentMeaningVersion)
	return queue.EnqueueAndRunInBackground(ctx, asyncjob.KindWordResearch, key, key, payload, WordResearchClaimTTL, WordResearchJobHandler(pipe, words))
}

func RunWordMeaningCleanupInline(ctx context.Context, pipe *pipeline.Pipeline, words wordreview.Store, userID string) error {
	return runWordMeaningCleanup(ctx, pipe, words, userID)
}
