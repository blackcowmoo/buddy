package llm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log"
	"time"

	"github.com/redis/go-redis/v9"
)

// DefaultCacheTTL bounds how long a cached Complete result is reused before
// the model is asked again. It's sized around asyncjob's retry semantics
// (see that package's doc comment): a crashed worker's job reruns "from
// scratch" once reaped, which — without this cache — recalls every
// pipeline.Pipeline.Analysis/Judge candidate again, even the ones that
// already succeeded on the failed attempt. A week is long enough to absorb
// that without adding a second knob to tune, short enough that a stale
// entry doesn't linger meaningfully past the correction/translation/
// compaction inputs it was keyed on.
const DefaultCacheTTL = 7 * 24 * time.Hour

// CachedClient wraps a Client so repeated Complete calls with identical
// model+input are served from Redis instead of invoking the model again.
// ChatStream passes straight through uncached: it's the live, streamed chat
// reply (pipeline.Pipeline.LLM), never meaningfully repeated with identical
// input the way an ensemble candidate's Complete call is.
type CachedClient struct {
	Client
	rdb redis.UniversalClient
	ttl time.Duration
}

// NewCached wraps inner with a Redis-backed Complete cache. rdb == nil
// disables caching (returns inner unchanged) — the same optional-Redis-
// feature convention as identity.NewCachedOIDCIdentifier and every other
// Redis-backed feature in this codebase. Any Redis error at call time falls
// through to calling inner directly, so a down/unreachable Redis degrades
// to "caching disabled," never a hard failure.
func NewCached(inner Client, rdb redis.UniversalClient, ttl time.Duration) Client {
	if rdb == nil {
		return inner
	}
	return &CachedClient{Client: inner, rdb: rdb, ttl: ttl}
}

func (c *CachedClient) Complete(ctx context.Context, model string, msgs []Message, jsonMode bool) (string, error) {
	key := completeCacheKey(model, msgs, jsonMode)
	if cached, err := c.rdb.Get(ctx, key).Result(); err == nil {
		return cached, nil
	} else if err != redis.Nil {
		log.Printf("llm: cache get: %v", err)
	}

	text, err := c.Client.Complete(ctx, model, msgs, jsonMode)
	if err != nil || text == "" {
		return text, err
	}
	if err := c.rdb.Set(ctx, key, text, c.ttl).Err(); err != nil {
		log.Printf("llm: cache set: %v", err)
	}
	return text, nil
}

func completeCacheKey(model string, msgs []Message, jsonMode bool) string {
	h := sha256.New()
	h.Write([]byte(model))
	h.Write([]byte{0})
	if jsonMode {
		h.Write([]byte{1})
	}
	h.Write([]byte{0})
	for _, m := range msgs {
		h.Write([]byte(m.Role))
		h.Write([]byte{0})
		h.Write([]byte(m.Content))
		h.Write([]byte{0})
	}
	return "llm:complete:" + hex.EncodeToString(h.Sum(nil))
}
