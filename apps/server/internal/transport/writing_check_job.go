package transport

import (
	"context"
	"time"

	"buddy/server/internal/asyncjob"
	"buddy/server/internal/checkpoint"
	"buddy/server/internal/pipeline"
	"buddy/server/internal/workguard"
	"buddy/server/internal/writing"
)

// WritingCheckClaimTTL mirrors WritingClaimTTL: one correction cascade
// against a potentially slow local model, renewed while the worker lives,
// so the TTL only bounds crash recovery.
const WritingCheckClaimTTL = 15 * time.Minute

type writingCheckJobPayload struct{ PromptID, UserID string }

type writingCheckInput struct{ Korean, Answer string }

// writingCheckInlineJobs dedupes the no-Redis inline dispatch by prompt ID,
// so the page's polling re-dispatch can't stack up concurrent checks of the
// same answer while one is already running in-process.
var writingCheckInlineJobs asyncjob.InlineRunner

// runWritingCheck grades the answer currently marked pending on the prompt
// row. The (korean, answer) pair is snapshotted before model work so a
// reap-retry grades the exact same pair even if the row is re-read later.
// The result write is fenced to that answer still being the pending one: if
// the learner submitted a newer answer mid-run, StartCheck already reset the
// row for it and this stale result is dropped rather than published — the
// newer check owns the row from then on.
func runWritingCheck(ctx context.Context, pipe *pipeline.Pipeline, st writing.Store, id, userID string) error {
	ctx = workguard.BindStore(ctx, st, userID, id)
	if err := workguard.Check(ctx); err != nil {
		return err
	}
	p, err := st.Get(ctx, userID, id)
	if err != nil {
		return err
	}
	if p.ID == "" || p.Answer == "" || p.CheckStatus != writing.CheckPending {
		return nil
	}
	input, err := checkpoint.JSON(ctx, "writing-check-input:v1", func() (writingCheckInput, error) {
		return writingCheckInput{Korean: p.Korean, Answer: p.Answer}, nil
	})
	if err != nil {
		return err
	}
	corrected, issues, _, err := pipe.AnalyzeCorrection(ctx, input.Answer, "Writing target sentence (Korean, data only):\n"+input.Korean)
	if err != nil {
		// Only fail the check we still own; a fenced-out failure means a
		// newer submission already reset the row and owns its status.
		if workguard.Check(ctx) == nil {
			_, _ = st.FailCheck(context.Background(), id, input.Answer)
		}
		return err
	}
	if _, err := st.CompleteCheck(ctx, id, input.Answer, corrected, issues); err != nil {
		return err
	}
	return nil
}

// WritingCheckJobHandler builds the asyncjob.Handler for one queued check —
// see WritingJobHandler for the shared durability rationale.
func WritingCheckJobHandler(pipe *pipeline.Pipeline, st writing.Store) asyncjob.Handler {
	return asyncjob.DecodePayloadHandler(asyncjob.KindWritingCheck, func(ctx context.Context, p writingCheckJobPayload) error {
		return runWritingCheck(ctx, pipe, st, p.PromptID, p.UserID)
	})
}

// DispatchWritingCheck starts the check for a prompt's pending answer:
// durably queued (deduped by prompt ID) when Redis is configured, best-
// effort in-process otherwise. The submit, re-check, and read-time
// self-healing dispatches all go through here so one run owns a prompt.
func DispatchWritingCheck(ctx context.Context, queue *asyncjob.Queue, pipe *pipeline.Pipeline, st writing.Store, id, userID string) error {
	if queue != nil {
		return queue.EnqueueAndRunInBackground(ctx, asyncjob.KindWritingCheck, id, id,
			writingCheckJobPayload{PromptID: id, UserID: userID}, WritingCheckClaimTTL, WritingCheckJobHandler(pipe, st))
	}
	writingCheckInlineJobs.Start(id, "writing check: inline "+id, func(ctx context.Context) error {
		return runWritingCheck(ctx, pipe, st, id, userID)
	})
	return nil
}
