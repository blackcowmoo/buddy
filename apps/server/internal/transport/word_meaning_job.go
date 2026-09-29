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
	type attempt struct {
		id       string
		revision int
	}
	visited := make(map[attempt]bool)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		list, err := store.PendingMeanings(ctx, userID)
		if err != nil {
			return err
		}
		processed := false
		for _, word := range list {
			key := attempt{word.ID, word.MeaningRevision}
			if visited[key] || word.Status != wordreview.StatusVerified || word.MeaningStatus != wordreview.MeaningPending || word.MeaningVersion >= wordreview.CurrentMeaningVersion || word.MeaningTargetVersion != wordreview.CurrentMeaningVersion {
				continue
			}
			visited[key], processed = true, true
			wordCtx := workguard.BindStore(ctx, words, userID, word.ID)
			meaning, err := wordMeaningForCleanup(wordCtx, pipe, word)
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
		// A learner can reject an earlier result while the rest of this batch
		// is still running. Drain those new revisions before releasing the job;
		// enqueueing the retry alone would deduplicate against this active run.
		if !processed {
			return nil
		}
	}
}

func wordMeaningForCleanup(ctx context.Context, pipe *pipeline.Pipeline, word wordreview.Word) (string, error) {
	// Version 2 is already in flight. Enable the cheaper path only when a
	// later meaning contract is deployed, without restarting today's work.
	if word.MeaningTargetVersion >= wordreview.MeaningOptimizationVersion {
		needsCleanup, err := pipe.NeedsWordMeaningCleanup(ctx, word.Word, word.Meaning, word.Example)
		if checkErr := workguard.Check(ctx); checkErr != nil {
			return "", checkErr
		}
		if err == nil && !needsCleanup {
			// SaveMeaning confirms an unchanged learner baseline, or preserves
			// a different original for the learner's choice.
			return word.Meaning, nil
		}
		// An unavailable or malformed preflight cannot certify a skip. Use
		// the existing normalization cascade to make the final decision.
	}
	return pipe.NormalizeWordMeaning(ctx, word.Word, word.Meaning, word.Example)
}

func EnqueueWordMeaningCleanup(ctx context.Context, queue *asyncjob.Queue, pipe *pipeline.Pipeline, words wordreview.Store, userID string) error {
	payload := wordResearchJobPayload{UserID: userID, CleanupMeanings: true}
	key := fmt.Sprintf("%s:meanings:v%d", userID, wordreview.CurrentMeaningVersion)
	return queue.EnqueueAndRunInBackground(ctx, asyncjob.KindWordResearch, key, key, payload, WordResearchClaimTTL, WordResearchJobHandler(pipe, words))
}

func RunWordMeaningCleanupInline(ctx context.Context, pipe *pipeline.Pipeline, words wordreview.Store, userID string) error {
	return runWordMeaningCleanup(ctx, pipe, words, userID)
}
