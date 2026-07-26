package stt

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"time"

	"github.com/redis/go-redis/v9"

	"buddy/server/internal/rediscache"
)

// DefaultCacheTTL mirrors llm.DefaultCacheTTL's reasoning for the STT
// ensemble (see pipeline.Pipeline.transcribe): a week is long enough that a
// reap-retried job (or any other accidental re-transcription of the exact
// same audio) is served from cache instead of paying for the model call
// again.
const DefaultCacheTTL = 7 * 24 * time.Hour

// CachedRecognizer wraps a Recognizer so repeated Transcribe calls with
// identical audio are served from Redis instead of transcribing again.
type CachedRecognizer struct {
	Recognizer
	rdb redis.UniversalClient
	ttl time.Duration
}

// NewCached wraps inner with a Redis-backed Transcribe cache. rdb == nil
// disables caching (returns inner unchanged), same convention as
// llm.NewCached. Any Redis error at call time falls through to calling
// inner directly.
func NewCached(inner Recognizer, rdb redis.UniversalClient, ttl time.Duration) Recognizer {
	if rdb == nil {
		return inner
	}
	return &CachedRecognizer{Recognizer: inner, rdb: rdb, ttl: ttl}
}

func (c *CachedRecognizer) Transcribe(ctx context.Context, pcm []byte) (Result, error) {
	key := transcribeCacheKey(c.Recognizer.Name(), pcm)
	return rediscache.GetOrSet(ctx, c.rdb, key, c.ttl, "stt",
		func() (Result, error) { return c.Recognizer.Transcribe(ctx, pcm) },
		func(res Result) bool { return res.Text == "" },
	)
}

func transcribeCacheKey(name string, pcm []byte) string {
	h := sha256.New()
	h.Write([]byte(name))
	h.Write([]byte{0})
	h.Write(pcm)
	return "stt:transcribe:" + hex.EncodeToString(h.Sum(nil))
}
