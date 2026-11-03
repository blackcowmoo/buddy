// Package quizcache owns the Redis representation of LLM semantic-similarity
// verdicts for quiz answers that the client could not match verbatim (see
// httpserver.quizAnswerCheckHandler). Verdicts live in Redis only: with no
// Redis there is no reuse at all, and every Redis failure degrades to a miss
// rather than failing the learner's check — the verdict is always recoverable
// from a Chat call, so caching is a latency optimization, not a source of
// truth.
package quizcache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// CacheTTL bounds a verdict's life. The MySQL answer cache this replaced kept
// true verdicts forever and false ones for a year; Redis gives both one
// bounded life, and httpserver renews a wrong verdict's TTL on every repeat
// hit so answers a learner keeps getting wrong stay cached while they keep
// reviewing them, instead of living forever because of one typo years ago.
const CacheTTL = 30 * 24 * time.Hour

const keyPrefix = "buddy:quiz-answer:v1:"

// Verdict is the terminal (post Analysis/Judge refinement) answer to one
// semantic-similarity check. Both fields carry the same bool because the
// refinement produces a single verdict; they mirror the two response fields
// the learner-facing endpoint returns, so a later split of "correct" from
// "similar" is a serialization change, not a schema change.
type Verdict struct {
	Correct bool `json:"correct"`
	Similar bool `json:"similar"`
}

// Key hashes the graded triple with the same normalization the retired MySQL
// cache used (trim + lowercase each part, NUL-separated) so a learner's
// casing/whitespace variants keep sharing one verdict.
func Key(prompt, answer, learnerAnswer string) string {
	h := sha256.New()
	for _, part := range []string{prompt, answer, learnerAnswer} {
		h.Write([]byte(strings.ToLower(strings.TrimSpace(part))))
		h.Write([]byte{0})
	}
	return keyPrefix + hex.EncodeToString(h.Sum(nil))
}

// Store is the capability the quiz-answer check path and its refine job
// need; *Cache implements it, and the interface lets callers accept a nil
// (Redis absent → no caching) without importing the Redis client themselves.
type Store interface {
	Lookup(ctx context.Context, key string) (Verdict, bool)
	Save(ctx context.Context, key string, v Verdict) error
	Renew(ctx context.Context, key string)
}

// Cache is a Store over the shared Redis client. A nil *Cache is a valid,
// disabled Store: every method is a no-op miss.
type Cache struct {
	rdb redis.UniversalClient
	ttl time.Duration
}

// New returns nil when rdb is nil, so callers can pass the result straight
// on as "caching disabled" like every other optional Redis feature here.
// The ttl parameter exists so tests can use a short TTL; production passes
// CacheTTL.
func New(rdb redis.UniversalClient, ttl time.Duration) *Cache {
	if rdb == nil {
		return nil
	}
	return &Cache{rdb: rdb, ttl: ttl}
}

func (c *Cache) Lookup(ctx context.Context, key string) (Verdict, bool) {
	if c == nil || key == "" {
		return Verdict{}, false
	}
	payload, err := c.rdb.Get(ctx, key).Bytes()
	if errors.Is(err, redis.Nil) {
		return Verdict{}, false
	}
	if err != nil {
		// Degraded Redis must not fail the learner's check; regenerate via
		// Chat and let a later Save reseat the entry.
		log.Printf("quizcache: lookup failed: %v", err)
		return Verdict{}, false
	}
	var v Verdict
	if err := json.Unmarshal(payload, &v); err != nil {
		log.Printf("quizcache: malformed entry: %v", err)
		return Verdict{}, false
	}
	return v, true
}

// Save returns the Redis error (unlike Lookup/Renew) because its only
// caller is the refine job, where a failed write should retry rather than
// silently drop an expensive Analysis/Judge result.
func (c *Cache) Save(ctx context.Context, key string, v Verdict) error {
	if c == nil || key == "" {
		return nil
	}
	payload, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return c.rdb.Set(ctx, key, payload, c.ttl).Err()
}

// Renew extends a cached verdict's TTL so a wrong answer the learner keeps
// re-hitting stays warm. EXPIRE only touches existing keys, so this can never
// create a TTL-less (or brand-new) entry; failures are logged, never fatal —
// the worst case is the verdict expiring on schedule.
func (c *Cache) Renew(ctx context.Context, key string) {
	if c == nil || key == "" {
		return
	}
	if err := c.rdb.Expire(ctx, key, c.ttl).Err(); err != nil {
		log.Printf("quizcache: renew failed: %v", err)
	}
}
