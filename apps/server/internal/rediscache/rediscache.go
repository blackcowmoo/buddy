// Package rediscache holds the get-or-compute-and-set wrapper shared by
// every "cache one expensive call's result in Redis" feature in this
// codebase (llm.CachedClient, stt.CachedRecognizer): try a Redis read
// first, fall back to actually doing the work on a miss, then best-effort
// write the result back with a TTL. Any Redis error, on either side, is
// logged and treated as a cache miss/no-op — never a hard failure — so a
// down/unreachable Redis always degrades to "caching disabled."
package rediscache

import (
	"context"
	"encoding/json"
	"log"
	"time"

	"github.com/redis/go-redis/v9"
)

// GetOrSet returns the JSON-decoded value cached at key, or calls compute
// and caches its result for ttl if there's no usable cache entry. A result
// for which skipCache returns true (e.g. an empty/zero result compute
// returned without an error) is returned but never written to the cache.
// logPrefix identifies the caller in any logged Redis error.
func GetOrSet[T any](ctx context.Context, rdb redis.UniversalClient, key string, ttl time.Duration, logPrefix string, compute func() (T, error), skipCache func(T) bool) (T, error) {
	if raw, err := rdb.Get(ctx, key).Bytes(); err == nil {
		var cached T
		if jsonErr := json.Unmarshal(raw, &cached); jsonErr == nil {
			return cached, nil
		}
	} else if err != redis.Nil {
		log.Printf("%s: cache get: %v", logPrefix, err)
	}

	result, err := compute()
	if err != nil || skipCache(result) {
		return result, err
	}
	if b, jsonErr := json.Marshal(result); jsonErr == nil {
		if err := rdb.Set(ctx, key, b, ttl).Err(); err != nil {
			log.Printf("%s: cache set: %v", logPrefix, err)
		}
	}
	return result, nil
}
