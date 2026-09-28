package llm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"

	"github.com/redis/go-redis/v9"

	"buddy/server/internal/checkpoint"
	"buddy/server/internal/rediscache"
)

// DefaultCacheTTL bounds reuse between independent requests. Durable jobs also
// keep their own checkpoints, which remain available throughout retries even
// after this optional cache expires.
const DefaultCacheTTL = 7 * 24 * time.Hour

// CachedClient wraps a Client so repeated Complete calls with identical
// endpoint+model+input are served from Redis instead of invoking the model again.
// ChatStream passes through: replay of a completed chat reply is scoped to its
// durable job by pipeline checkpoints, never shared with independent requests.
type CachedClient struct {
	Client
	rdb redis.UniversalClient
	ttl time.Duration
}

// QueueKey preserves the wrapped client's endpoint-aware queue identity. A
// Redis cache must not make two OpenAI endpoints serving the same model share
// a queue merely because the cache wrapper hides the underlying concrete
// client from Pipeline.
func (c *CachedClient) QueueKey(model string) string {
	if keyed, ok := c.Client.(QueueKeyer); ok {
		return keyed.QueueKey(model)
	}
	return model
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
	// Job checkpoints validate responses before retaining them. Reading this
	// unvalidated cache could otherwise pin a rejected result on every retry.
	if checkpoint.Bound(ctx) {
		return c.Client.Complete(ctx, model, msgs, jsonMode)
	}
	key := completeCacheKey(c.QueueKey(model), model, msgs, jsonMode)
	return rediscache.GetOrSet(ctx, c.rdb, key, c.ttl, "llm",
		func() (string, error) { return c.Client.Complete(ctx, model, msgs, jsonMode) },
		func(text string) bool { return text == "" },
	)
}

func completeCacheKey(endpoint, model string, msgs []Message, jsonMode bool) string {
	input, _ := json.Marshal(struct {
		Endpoint string
		Model    string
		Messages []Message
		JSON     bool
	}{endpoint, model, msgs, jsonMode})
	digest := sha256.Sum256(input)
	return "llm:complete:v2:" + hex.EncodeToString(digest[:])
}
