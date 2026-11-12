package transport

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"

	"buddy/server/internal/asyncjob"
	"buddy/server/internal/newsarticle"
	"buddy/server/internal/pipeline"
	"buddy/server/internal/protocol"
	"buddy/server/internal/wordlookup"
	"buddy/server/internal/workguard"
)

// Word define (a tapped word's full definition — see pipeline.DefineWord) and
// phrase hint (a tapped phrase's non-revealing nudge — see pipeline.PhraseHint)
// are the same durable job: "the learner's GET found the wordlookup cache
// empty; generate the entry and seat it." They share the payload, the run
// body below, and one enqueue helper shape. They stay separate Kinds with
// their own concurrency so a tap-triggered hint queue can never delay an
// explicit word search behind it (see asyncjob.KindPhraseHint); the shared
// 25h claim TTL reflects one call against a potentially slow local model
// (see WordDefineClaimTTL's original reasoning, kept here for both).
const (
	WordDefineClaimTTL          = 25 * time.Hour
	WordDefineWorkerConcurrency = 8
	// PhraseHintWorkerConcurrency stays below word define's: hints fire
	// automatically from phrase taps, so an explicit search the learner
	// asked for gets the first claim on model capacity.
	PhraseHintClaimTTL          = 25 * time.Hour
	PhraseHintWorkerConcurrency = 4
)

// lookupJobPayload is the wire format of both lookup Kinds — each producer
// fills it from wordlookup's request identity (Key for a definition, HintKey
// for a hint), and the job envelope's Kind, not the payload, says which
// lookup to run.
type lookupJobPayload struct {
	CacheKey string
	Request  wordlookup.Request
}

// runLookupJob is the shared body of both lookup handlers: skip when the
// entry already landed (another replica or an earlier attempt won the race),
// otherwise generate and cache it. label prefixes every error so a failed
// job still names its lookup, and T is the cached value type so each Kind
// keeps decoding exactly its own cache contract.
func runLookupJob[T any](ctx context.Context, label string, rdb redis.UniversalClient, payload lookupJobPayload, articles newsarticle.Store, generate func(context.Context, wordlookup.Request) (T, error)) error {
	ctx = workguard.BindStore(ctx, articles, "", payload.Request.ArticleID)
	if err := workguard.Check(ctx); err != nil {
		return err
	}
	if rdb == nil {
		return fmt.Errorf("%s: redis is unavailable", label)
	}
	if _, ok, err := wordlookup.Get[T](ctx, rdb, payload.CacheKey); err != nil {
		return fmt.Errorf("%s: cache read: %w", label, err)
	} else if ok {
		return nil
	}
	result, err := generate(ctx, payload.Request)
	if err != nil {
		return fmt.Errorf("%s: generate: %w", label, err)
	}
	if err := wordlookup.Set(ctx, rdb, payload.CacheKey, result); err != nil {
		return fmt.Errorf("%s: cache write: %w", label, err)
	}
	return nil
}

func WordDefineJobHandler(pipe *pipeline.Pipeline, rdb redis.UniversalClient, articles newsarticle.Store) asyncjob.Handler {
	return asyncjob.DecodePayloadHandler(asyncjob.KindWordDefine, func(ctx context.Context, payload lookupJobPayload) error {
		return runLookupJob(ctx, "word define", rdb, payload, articles, func(ctx context.Context, req wordlookup.Request) (protocol.WordSuggestion, error) {
			return pipe.DefineWord(ctx, req.Word, req.Context)
		})
	})
}

func EnqueueWordDefineJob(ctx context.Context, queue *asyncjob.Queue, pipe *pipeline.Pipeline, rdb redis.UniversalClient, req wordlookup.Request, articles newsarticle.Store) error {
	payload := lookupJobPayload{CacheKey: wordlookup.Key(req), Request: req}
	key := payload.CacheKey
	return queue.EnqueueAndRunInBackground(ctx, asyncjob.KindWordDefine, key, key, payload, WordDefineClaimTTL, WordDefineJobHandler(pipe, rdb, articles))
}

func PhraseHintJobHandler(pipe *pipeline.Pipeline, rdb redis.UniversalClient, articles newsarticle.Store) asyncjob.Handler {
	return asyncjob.DecodePayloadHandler(asyncjob.KindPhraseHint, func(ctx context.Context, payload lookupJobPayload) error {
		return runLookupJob(ctx, "phrase hint", rdb, payload, articles, func(ctx context.Context, req wordlookup.Request) (wordlookup.Hint, error) {
			hint, err := pipe.PhraseHint(ctx, req.Word, req.Context)
			return wordlookup.Hint{Hint: hint}, err
		})
	})
}

func EnqueuePhraseHintJob(ctx context.Context, queue *asyncjob.Queue, pipe *pipeline.Pipeline, rdb redis.UniversalClient, req wordlookup.Request, articles newsarticle.Store) error {
	payload := lookupJobPayload{CacheKey: wordlookup.HintKey(req), Request: req}
	key := payload.CacheKey
	return queue.EnqueueAndRunInBackground(ctx, asyncjob.KindPhraseHint, key, key, payload, PhraseHintClaimTTL, PhraseHintJobHandler(pipe, rdb, articles))
}
