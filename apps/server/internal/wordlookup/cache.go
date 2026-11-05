// Package wordlookup owns the Redis representation of context-aware article
// word lookups. The key includes the complete reading context, not just the
// spelling, because one word can have different meanings in different
// articles or positions. Its version also follows the definition prompt's
// output contract so a prompt upgrade does not keep serving stale meanings.
package wordlookup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	CacheTTL     = 30 * 24 * time.Hour
	cacheVersion = "v2" // concise, memorization-ready meaning contract
	hintVersion  = "v1" // non-revealing pre-search hint contract
)

type Request struct {
	ArticleID string `json:"articleId"`
	Word      string `json:"word"`
	Position  int    `json:"position"`
	Context   string `json:"context"`
	Language  string `json:"language"`
	Model     string `json:"model"`
}

func Key(r Request) string {
	return "buddy:word-lookup:" + cacheVersion + ":" + requestHash(r)
}

// Hint is the pre-search meaning nudge for one tapped article phrase. It
// deliberately does not carry the memorizable gloss WordSuggestion.Meaning
// holds, so it lives in its own cache namespace below.
type Hint struct {
	Hint string `json:"hint"`
}

// HintKey is the cache key for one phrase's hint. Same request identity as
// Key (a hint is resolved in the same article context) but a separate
// namespace, so a hint is never served as — or overwritten by — the full
// definition and each can be regenerated under its own prompt contract.
func HintKey(r Request) string {
	return "buddy:phrase-hint:" + hintVersion + ":" + requestHash(r)
}

func requestHash(r Request) string {
	b, _ := json.Marshal(r)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// Get/Set store one cached value as JSON. Definitions and hints never share
// a namespace (see Key and HintKey), so the codec itself is type-agnostic
// and one implementation serves every cached kind.
func Get[T any](ctx context.Context, rdb redis.UniversalClient, key string) (T, bool, error) {
	var result T
	raw, err := rdb.Get(ctx, key).Bytes()
	if err == redis.Nil {
		return result, false, nil
	}
	if err != nil {
		return result, false, err
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return result, false, err
	}
	return result, true, nil
}

func Set[T any](ctx context.Context, rdb redis.UniversalClient, key string, result T) error {
	b, err := json.Marshal(result)
	if err != nil {
		return err
	}
	return rdb.Set(ctx, key, b, CacheTTL).Err()
}
