package transport

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"

	"buddy/server/internal/asyncjob"
	"buddy/server/internal/pipeline"
	"buddy/server/internal/wordlookup"
)

const (
	WordDefineClaimTTL          = 25 * time.Hour
	WordDefineWorkerConcurrency = 8
)

type wordDefineJobPayload struct {
	CacheKey string
	Request  wordlookup.Request
}

func runWordDefine(ctx context.Context, pipe *pipeline.Pipeline, rdb redis.UniversalClient, payload wordDefineJobPayload) error {
	if rdb == nil {
		return fmt.Errorf("word define: redis is unavailable")
	}
	if _, ok, err := wordlookup.Get(ctx, rdb, payload.CacheKey); err != nil {
		return fmt.Errorf("word define: cache read: %w", err)
	} else if ok {
		return nil
	}
	result, err := pipe.DefineWord(ctx, payload.Request.Word, payload.Request.Context)
	if err != nil {
		return fmt.Errorf("word define: generate: %w", err)
	}
	if err := wordlookup.Set(ctx, rdb, payload.CacheKey, result); err != nil {
		return fmt.Errorf("word define: cache write: %w", err)
	}
	return nil
}

func WordDefineJobHandler(pipe *pipeline.Pipeline, rdb redis.UniversalClient) asyncjob.Handler {
	return asyncjob.DecodePayloadHandler(asyncjob.KindWordDefine, func(ctx context.Context, payload wordDefineJobPayload) error {
		return runWordDefine(ctx, pipe, rdb, payload)
	})
}

func EnqueueWordDefineJob(ctx context.Context, queue *asyncjob.Queue, pipe *pipeline.Pipeline, rdb redis.UniversalClient, req wordlookup.Request) error {
	payload := wordDefineJobPayload{CacheKey: wordlookup.Key(req), Request: req}
	key := payload.CacheKey
	return queue.EnqueueAndRunInBackground(ctx, asyncjob.KindWordDefine, key, key, payload, WordDefineClaimTTL, WordDefineJobHandler(pipe, rdb))
}

func RunWordDefineInline(ctx context.Context, pipe *pipeline.Pipeline, rdb redis.UniversalClient, req wordlookup.Request) error {
	return runWordDefine(ctx, pipe, rdb, wordDefineJobPayload{CacheKey: wordlookup.Key(req), Request: req})
}

func dispatchWordDefine(ctx context.Context, queue *asyncjob.Queue, pipe *pipeline.Pipeline, rdb redis.UniversalClient, req wordlookup.Request) {
	asyncjob.EnqueueOrRunInline(queue, ctx,
		"words: enqueue article lookup "+req.ArticleID,
		func(ctx context.Context) error { return EnqueueWordDefineJob(ctx, queue, pipe, rdb, req) },
		"words: article lookup "+req.ArticleID,
		func(ctx context.Context) error { return RunWordDefineInline(ctx, pipe, rdb, req) },
	)
}

// StartWordDefine is intentionally detached from the request. It is kept
// small so the HTTP layer cannot accidentally pass its request context into
// the LLM call.
func StartWordDefine(queue *asyncjob.Queue, pipe *pipeline.Pipeline, rdb redis.UniversalClient, req wordlookup.Request) {
	go dispatchWordDefine(context.Background(), queue, pipe, rdb, req)
}
