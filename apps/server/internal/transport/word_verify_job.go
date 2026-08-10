package transport

import (
	"context"
	"fmt"
	"time"

	"buddy/server/internal/asyncjob"
	"buddy/server/internal/pipeline"
	"buddy/server/internal/wordreview"
)

// WordVerifyClaimTTL/WordVerifyWorkerConcurrency mirror StudyQuizClaimTTL/
// StudyQuizWorkerConcurrency's reasoning, except this job makes
// minWordVerifyJudges (pipeline.go) LLM calls instead of one — a local model
// can be slow, so the claim TTL stays generous. Concurrency is a bit higher
// than the study-quiz/summary jobs since "학습하기" can be tapped repeatedly
// in quick succession while browsing search results, unlike ending a
// conversation (at most one in flight per learner).
const (
	WordVerifyClaimTTL          = 25 * time.Hour
	WordVerifyWorkerConcurrency = 8
)

// wordVerifyJobPayload mirrors studyQuizJobPayload: just the IDs, not the
// word/meaning/example themselves, so a reap-retry always verifies whatever
// is actually persisted rather than a payload that could be stale.
type wordVerifyJobPayload struct {
	UserID string
	WordID string
}

// wordVerifyKey formats the userID:wordID dedupe/log key for this package's
// word-verify job — same plain-string-key idea as turnKey/turnLogID, just
// keyed by word id instead of session+turn since there's no turn concept
// here.
func wordVerifyKey(userID, wordID string) string {
	return userID + ":" + wordID
}

// runWordVerify is the actual work behind asyncjob.KindWordVerify: look up
// the pending word, ask pipeline.Pipeline.VerifyWord to fact-check it, and
// record the outcome. Shared by WordVerifyJobHandler (the queued, durable
// path) and RunWordVerifyInline (httpserver.wordSaveHandler's fallback when
// Redis isn't configured) so both paths behave identically. A word that no
// longer exists (e.g. the learner deleted it while verification was still
// pending) is treated as already done — nothing left to verify, not an
// error worth retrying.
func runWordVerify(ctx context.Context, pipe *pipeline.Pipeline, words wordreview.Store, userID, wordID string) error {
	target, err := words.Get(ctx, userID, wordID)
	if err != nil {
		return fmt.Errorf("word verify: get: %w", err)
	}
	if target.ID == "" || target.Status != wordreview.StatusPending {
		return nil
	}

	valid, reason, err := pipe.VerifyWord(ctx, target.Word, target.Meaning, target.Example)
	if err != nil {
		return fmt.Errorf("word verify: %w", err)
	}
	if valid {
		if _, err := words.MarkVerified(ctx, userID, wordID, time.Now()); err != nil {
			return fmt.Errorf("word verify: mark verified: %w", err)
		}
		return nil
	}
	if _, err := words.MarkRejected(ctx, userID, wordID, reason); err != nil {
		return fmt.Errorf("word verify: mark rejected: %w", err)
	}
	return nil
}

// WordVerifyJobHandler builds the asyncjob.Handler that runs one queued
// word-verify job, independent of any connection or its context — see
// StudySummaryJobHandler's doc comment for the shared durability rationale.
func WordVerifyJobHandler(pipe *pipeline.Pipeline, words wordreview.Store) asyncjob.Handler {
	return asyncjob.DecodePayloadHandler(asyncjob.KindWordVerify, func(ctx context.Context, payload wordVerifyJobPayload) error {
		return runWordVerify(ctx, pipe, words, payload.UserID, payload.WordID)
	})
}

// RunWordVerifyInline runs the exact same work as WordVerifyJobHandler,
// synchronously — mirrors RunStudyQuizInline; the caller is expected to run
// this on a detached context.Background() goroutine of its own so a slow
// local LLM never blocks the "학습하기" response.
func RunWordVerifyInline(ctx context.Context, pipe *pipeline.Pipeline, words wordreview.Store, userID, wordID string) error {
	return runWordVerify(ctx, pipe, words, userID, wordID)
}

// EnqueueWordVerifyJob durably queues verification for a just-saved pending
// word — mirrors EnqueueStudyQuizJob.
func EnqueueWordVerifyJob(ctx context.Context, queue *asyncjob.Queue, pipe *pipeline.Pipeline, words wordreview.Store, userID, wordID string) error {
	payload := wordVerifyJobPayload{UserID: userID, WordID: wordID}
	key := wordVerifyKey(userID, wordID)
	return queue.EnqueueAndRunInBackground(ctx, asyncjob.KindWordVerify, key,
		key, payload, WordVerifyClaimTTL, WordVerifyJobHandler(pipe, words))
}

// SaveWordAndVerify saves one word/meaning/example for userID and, if it
// came back freshly StatusPending, kicks off its model-consensus check — the
// shared "save, then verify in the background" step every path that adds a
// word needs, whether it's one learner-picked word (httpserver.
// wordSaveHandler) or a batch of system-suggested ones (runWordAutoAdd in
// word_auto_add_job.go), so a system-suggested word is fact-checked exactly
// the same way a manually picked one is, no shortcut.
func SaveWordAndVerify(ctx context.Context, words wordreview.Store, pipe *pipeline.Pipeline, wordVerifyQueue *asyncjob.Queue, userID, word, meaning, example string) (wordreview.Word, error) {
	word = wordreview.NormalizeWord(word)
	saved, err := words.Save(ctx, userID, word, meaning, example)
	if err != nil {
		return wordreview.Word{}, err
	}
	if saved.Status == wordreview.StatusPending {
		asyncjob.EnqueueOrRunInline(wordVerifyQueue, ctx,
			"words: enqueue verify "+userID+"/"+saved.ID,
			func(ctx context.Context) error {
				return EnqueueWordVerifyJob(ctx, wordVerifyQueue, pipe, words, userID, saved.ID)
			},
			"words: verify "+userID+"/"+saved.ID,
			func(ctx context.Context) error {
				return RunWordVerifyInline(ctx, pipe, words, userID, saved.ID)
			},
		)
	}
	return saved, nil
}
