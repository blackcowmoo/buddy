package transport

import (
	"context"
	"encoding/json"
	"time"

	"buddy/server/internal/asyncjob"
	"buddy/server/internal/pipeline"
	"buddy/server/internal/writing"
)

const (
	// A writing prompt is one model call. Keep enough time for the slow local
	// model, while allowing a new process to recover a claim after the old
	// process died. asyncjob renews this lease while the handler is alive, so
	// this is a crash-recovery bound rather than a maximum runtime.
	WritingClaimTTL          = 15 * time.Minute
	WritingWorkerConcurrency = 4
)

type writingJobPayload struct{ PromptID, UserID string }

func generateWritingPrompt(ctx context.Context, pipe *pipeline.Pipeline, st writing.Store, id, userID, profile string) error {
	p, err := st.Get(ctx, userID, id)
	if err != nil {
		return err
	}
	// Failed is retryable: asyncjob reaps a failed attempt and invokes the
	// same payload again, so the row must be allowed back through generation.
	if p.ID == "" || (p.Status != writing.StatusPending && p.Status != writing.StatusFailed) {
		return nil
	}
	previousPrompts, err := st.List(ctx, userID)
	if err != nil {
		return err
	}
	previous := make([]string, 0, len(previousPrompts))
	for _, old := range previousPrompts {
		if old.Korean != "" {
			previous = append(previous, old.Korean)
		}
	}
	result, err := pipe.GenerateWritingPrompt(ctx, profile, previous, id)
	if err != nil {
		_ = st.Fail(context.Background(), id)
		return err
	}
	return st.Complete(ctx, id, result.Korean)
}

func WritingJobHandler(pipe *pipeline.Pipeline, st writing.Store, profile func(context.Context, string) (string, error)) asyncjob.Handler {
	return asyncjob.DecodePayloadHandler(asyncjob.KindWritingPrompt, func(ctx context.Context, p writingJobPayload) error {
		profileText, err := profile(ctx, p.UserID)
		if err != nil {
			_ = st.Fail(context.Background(), p.PromptID)
			return err
		}
		return generateWritingPrompt(ctx, pipe, st, p.PromptID, p.UserID, profileText)
	})
}

func RunWritingPromptInline(ctx context.Context, pipe *pipeline.Pipeline, st writing.Store, profile func(context.Context, string) (string, error), id, userID string) error {
	profileText, err := profile(ctx, userID)
	if err != nil {
		_ = st.Fail(context.Background(), id)
		return err
	}
	return generateWritingPrompt(ctx, pipe, st, id, userID, profileText)
}

func mustPayload(v any) []byte { b, _ := json.Marshal(v); return b }

func EnqueueWritingPromptJob(ctx context.Context, q *asyncjob.Queue, pipe *pipeline.Pipeline, st writing.Store, profile func(context.Context, string) (string, error), id, userID string) error {
	return q.EnqueueAndRunInBackground(ctx, asyncjob.KindWritingPrompt, id, id, writingJobPayload{PromptID: id, UserID: userID}, WritingClaimTTL, WritingJobHandler(pipe, st, profile))
}
