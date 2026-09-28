package transport

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"buddy/server/internal/asyncjob"
	"buddy/server/internal/checkpoint"
	"buddy/server/internal/pipeline"
	"buddy/server/internal/protocol"
	"buddy/server/internal/store"
	"buddy/server/internal/wordreview"
	"buddy/server/internal/workguard"
)

// WordAutoAddClaimTTL/WordAutoAddWorkerConcurrency mirror ArticleStudyClaimTTL/
// ArticleStudyWorkerConcurrency's reasoning: one SuggestNewWords call plus
// each saved word's own KindWordVerify fact-check against a possibly slow
// local model, so the claim TTL stays generous. Concurrency is modest —
// "새 단어 추가로 학습하기" is a deliberate, occasional action like drawing a
// fresh article, not something tapped repeatedly like "학습하기".
const (
	WordAutoAddClaimTTL          = 25 * time.Hour
	WordAutoAddWorkerConcurrency = 4
)

// maxAutoAddExclusionWords caps how many already-tracked words get listed in
// pipe.SuggestNewWords' prompt, keeping the newest ones (words.List returns
// most-recently-added first, the likeliest near-duplicates of a fresh
// suggestion).
const maxAutoAddExclusionWords = 200

// wordAutoAddJobPayload carries just the userID. The first execution snapshots
// current inputs; retries retain that draw's inputs and proposed batch even
// when some of its words were already saved by an interrupted attempt.
type wordAutoAddJobPayload struct {
	UserID string
}

// generateAndSaveAutoAddWords asks pipe.SuggestNewWords for candidates fit
// to the learner's profile and saves each valid one through
// SaveWordAndVerify, returning how many were actually added. Suggestions
// that fail basic validation (empty, or over the column-length caps) are
// skipped, not fatal to the batch — mirrors the per-suggestion filtering
// httpserver.wordAutoAddHandler used to do inline before this became a
// background job.
func generateAndSaveAutoAddWords(ctx context.Context, pipe *pipeline.Pipeline, words wordreview.Store, st store.Store, wordVerifyQueue *asyncjob.Queue, userID string) (int, error) {
	// Persist the proposed batch before its first write. Re-reading exclusions
	// after a partial save would otherwise turn a retry into another new draw.
	suggestions, err := checkpoint.JSON(ctx, "word-auto-add-proposals:v1", func() ([]protocol.WordSuggestion, error) {
		type generationInput struct {
			Profile  string
			Existing []string
		}
		input, err := checkpoint.JSON(ctx, "word-auto-add-input:v1", func() (generationInput, error) {
			profile, err := st.GetLearnerProfile(ctx, userID)
			if err != nil {
				return generationInput{}, fmt.Errorf("word auto-add: get learner profile: %w", err)
			}
			tracked, err := words.List(ctx, userID)
			if err != nil {
				return generationInput{}, fmt.Errorf("word auto-add: list: %w", err)
			}
			if len(tracked) > maxAutoAddExclusionWords {
				tracked = tracked[:maxAutoAddExclusionWords]
			}
			existing := make([]string, len(tracked))
			for i, t := range tracked {
				existing[i] = t.Word
			}
			return generationInput{Profile: profile, Existing: existing}, nil
		})
		if err != nil {
			return nil, err
		}
		proposals, err := pipe.SuggestNewWords(ctx, input.Profile, input.Existing)
		if err != nil {
			return nil, fmt.Errorf("word auto-add: suggest: %w", err)
		}
		valid := make([]protocol.WordSuggestion, 0, len(proposals))
		for _, s := range proposals {
			s.Word = strings.TrimSpace(s.Word)
			s.Meaning = strings.TrimSpace(s.Meaning)
			s.Example = strings.TrimSpace(s.Example)
			if wordreview.ValidateFields(s.Word, s.Meaning, s.Example) == nil {
				valid = append(valid, s)
			}
		}
		return valid, nil
	})
	if err != nil {
		return 0, err
	}

	added := 0
	for _, s := range suggestions {
		if err := workguard.Check(ctx); err != nil {
			return added, err
		}
		if _, err := SaveWordAndVerify(ctx, words, pipe, wordVerifyQueue, userID, s.Word, s.Meaning, s.Example, s.Word); err != nil {
			return added, fmt.Errorf("word auto-add: save: %w", err)
		}
		added++
	}
	return added, nil
}

// runWordAutoAdd is the actual work behind asyncjob.KindWordAutoAdd:
// re-check the job is still JobStatusPending (a no-op otherwise — already
// completed by an earlier attempt, or a racing concurrent click), generate
// and save a batch of new words, then record the outcome. A failed
// generation marks the run JobStatusFailed and returns the error so
// asyncjob's own retry bookkeeping observes the failure, but — unlike a
// still-pending job — a later reap attempt sees JobStatusFailed here and
// treats it as already-terminal (no-op), the same as
// httpserver.articleDrawHandler's runArticleStudy does for
// newsarticle.StatusFailed; the learner has to press "새 단어 추가로
// 학습하기" again to actually retry.
func runWordAutoAdd(ctx context.Context, pipe *pipeline.Pipeline, words wordreview.Store, st store.Store, wordVerifyQueue *asyncjob.Queue, userID string) error {
	status, _, err := st.GetWordAutoAddStatus(ctx, userID)
	if err != nil {
		return fmt.Errorf("word auto-add: status: %w", err)
	}
	if status != store.JobStatusPending {
		return nil
	}

	added, err := generateAndSaveAutoAddWords(ctx, pipe, words, st, wordVerifyQueue, userID)
	if err != nil {
		if checkErr := workguard.Check(ctx); checkErr != nil {
			return checkErr
		}
		if failErr := st.FailWordAutoAdd(context.Background(), userID); failErr != nil {
			log.Printf("word auto-add: fail %s: %v", userID, failErr)
		}
		return err
	}
	if err := workguard.Check(ctx); err != nil {
		return err
	}
	if err := st.CompleteWordAutoAdd(ctx, userID, added); err != nil {
		return fmt.Errorf("word auto-add: complete: %w", err)
	}
	return nil
}

// WordAutoAddJobHandler builds the asyncjob.Handler that runs one queued
// word-auto-add job, independent of any connection or its context — see
// ArticleStudyJobHandler's doc comment for the shared durability rationale.
func WordAutoAddJobHandler(pipe *pipeline.Pipeline, words wordreview.Store, st store.Store, wordVerifyQueue *asyncjob.Queue) asyncjob.Handler {
	return asyncjob.DecodePayloadHandler(asyncjob.KindWordAutoAdd, func(ctx context.Context, payload wordAutoAddJobPayload) error {
		return runWordAutoAdd(ctx, pipe, words, st, wordVerifyQueue, payload.UserID)
	})
}

// RunWordAutoAddInline runs the exact same work as WordAutoAddJobHandler,
// synchronously — mirrors RunArticleStudyInline; the caller is expected to
// run this on a detached context.Background() goroutine of its own so a
// slow LLM call never blocks httpserver.wordAutoAddHandler's response.
func RunWordAutoAddInline(ctx context.Context, pipe *pipeline.Pipeline, words wordreview.Store, st store.Store, wordVerifyQueue *asyncjob.Queue, userID string) error {
	return runWordAutoAdd(ctx, pipe, words, st, wordVerifyQueue, userID)
}

// EnqueueWordAutoAddJob durably queues auto-add generation for userID —
// mirrors EnqueueArticleStudyJob. Deduped by userID, so a double-tap (or a
// second request racing the same click before StartWordAutoAdd's row is
// even visible) only ever triggers one generation.
func EnqueueWordAutoAddJob(ctx context.Context, queue *asyncjob.Queue, pipe *pipeline.Pipeline, words wordreview.Store, st store.Store, wordVerifyQueue *asyncjob.Queue, userID string) error {
	payload := wordAutoAddJobPayload{UserID: userID}
	return queue.EnqueueAndRunInBackground(ctx, asyncjob.KindWordAutoAdd, userID,
		userID, payload, WordAutoAddClaimTTL, WordAutoAddJobHandler(pipe, words, st, wordVerifyQueue))
}
