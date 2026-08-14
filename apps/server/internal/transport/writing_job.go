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
	WritingClaimTTL          = 25 * time.Hour
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
	result, err := pipe.GenerateWritingPrompt(ctx, profile)
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
