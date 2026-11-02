package transport

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"

	"buddy/server/internal/asyncjob"
	"buddy/server/internal/newsarticle"
	"buddy/server/internal/pipeline"
	"buddy/server/internal/wordlookup"
	"buddy/server/internal/workguard"
)

// PhraseHintClaimTTL follows WordDefineClaimTTL's reasoning (one call
// against a potentially slow local model). Concurrency stays below word
// define's: hints fire automatically from phrase taps, so an explicit
// search the learner asked for gets the first claim on model capacity.
const (
	PhraseHintClaimTTL          = 25 * time.Hour
	PhraseHintWorkerConcurrency = 4
)

type phraseHintJobPayload struct {
	CacheKey string
	Request  wordlookup.Request
}

func runPhraseHint(ctx context.Context, pipe *pipeline.Pipeline, rdb redis.UniversalClient, payload phraseHintJobPayload, articles newsarticle.Store) error {
	ctx = workguard.BindStore(ctx, articles, "", payload.Request.ArticleID)
	if err := workguard.Check(ctx); err != nil {
		return err
	}
	if rdb == nil {
		return fmt.Errorf("phrase hint: redis is unavailable")
	}
	if _, ok, err := wordlookup.GetHint(ctx, rdb, payload.CacheKey); err != nil {
		return fmt.Errorf("phrase hint: cache read: %w", err)
	} else if ok {
		return nil
	}
	hint, err := pipe.PhraseHint(ctx, payload.Request.Word, payload.Request.Context)
	if err != nil {
		return fmt.Errorf("phrase hint: generate: %w", err)
	}
	if err := wordlookup.SetHint(ctx, rdb, payload.CacheKey, wordlookup.Hint{Hint: hint}); err != nil {
		return fmt.Errorf("phrase hint: cache write: %w", err)
	}
	return nil
}

func PhraseHintJobHandler(pipe *pipeline.Pipeline, rdb redis.UniversalClient, articles newsarticle.Store) asyncjob.Handler {
	return asyncjob.DecodePayloadHandler(asyncjob.KindPhraseHint, func(ctx context.Context, payload phraseHintJobPayload) error {
		return runPhraseHint(ctx, pipe, rdb, payload, articles)
	})
}

func EnqueuePhraseHintJob(ctx context.Context, queue *asyncjob.Queue, pipe *pipeline.Pipeline, rdb redis.UniversalClient, req wordlookup.Request, articles newsarticle.Store) error {
	payload := phraseHintJobPayload{CacheKey: wordlookup.HintKey(req), Request: req}
	key := payload.CacheKey
	return queue.EnqueueAndRunInBackground(ctx, asyncjob.KindPhraseHint, key, key, payload, PhraseHintClaimTTL, PhraseHintJobHandler(pipe, rdb, articles))
}
