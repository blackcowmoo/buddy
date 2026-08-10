// Package wordlookup owns the Redis representation of context-aware article
// word lookups. The key includes the complete reading context, not just the
// spelling, because one word can have different meanings in different
// articles or positions.
package wordlookup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"

	"github.com/redis/go-redis/v9"

	"buddy/server/internal/protocol"
)

const CacheTTL = 30 * 24 * time.Hour

type Request struct {
	ArticleID string `json:"articleId"`
	Word      string `json:"word"`
	Position  int    `json:"position"`
	Context   string `json:"context"`
	Language  string `json:"language"`
	Model     string `json:"model"`
}

func Key(r Request) string {
	b, _ := json.Marshal(r)
	h := sha256.Sum256(b)
	return "buddy:word-lookup:" + hex.EncodeToString(h[:])
}

func Get(ctx context.Context, rdb redis.UniversalClient, key string) (protocol.WordSuggestion, bool, error) {
	var result protocol.WordSuggestion
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

func Set(ctx context.Context, rdb redis.UniversalClient, key string, result protocol.WordSuggestion) error {
	b, err := json.Marshal(result)
	if err != nil {
		return err
	}
	return rdb.Set(ctx, key, b, CacheTTL).Err()
}
