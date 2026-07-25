package stt

import (
	"context"
	"errors"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"
)

// sharedRedis backs every test in this file — see llm/cached_test.go's
// sharedRedis for the same reasoning.
var (
	sharedRedis    *redis.Client
	sharedRedisErr error
)

func TestMain(m *testing.M) {
	os.Exit(runSTTTests(m))
}

func runSTTTests(m *testing.M) int {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	container, err := tcredis.Run(ctx, "redis:7")
	if err != nil {
		sharedRedisErr = err
		return m.Run()
	}
	defer func() { _ = container.Terminate(context.Background()) }()

	connStr, err := container.ConnectionString(ctx)
	if err != nil {
		sharedRedisErr = err
		return m.Run()
	}
	opts, err := redis.ParseURL(connStr)
	if err != nil {
		sharedRedisErr = err
		return m.Run()
	}
	sharedRedis = redis.NewClient(opts)
	defer func() { _ = sharedRedis.Close() }()

	return m.Run()
}

func requireRedis(t *testing.T) *redis.Client {
	t.Helper()
	if sharedRedisErr != nil {
		t.Skipf("redis testcontainer unavailable (no/unreachable Docker?): %v", sharedRedisErr)
	}
	return sharedRedis
}

// countingRecognizer counts Transcribe calls so tests can assert a cache hit
// never reaches the underlying engine.
type countingRecognizer struct {
	transcribes atomic.Int32
	name        string
	result      Result
	err         error
}

func (r *countingRecognizer) Name() string { return r.name }

func (r *countingRecognizer) Transcribe(ctx context.Context, pcm []byte) (Result, error) {
	r.transcribes.Add(1)
	return r.result, r.err
}

func TestCachedRecognizerServesSecondCallFromCacheWithoutCallingInner(t *testing.T) {
	rdb := requireRedis(t)
	inner := &countingRecognizer{name: "whisper", result: Result{Text: "hello there", Confidence: 0.9}}
	cached := NewCached(inner, rdb, time.Minute)

	pcm := []byte{1, 2, 3, 4}

	got, err := cached.Transcribe(context.Background(), pcm)
	if err != nil || got.Text != "hello there" {
		t.Fatalf("first Transcribe() = %+v, %v; want text %q", got, err, "hello there")
	}
	if n := inner.transcribes.Load(); n != 1 {
		t.Fatalf("inner.transcribes after first call = %d, want 1", n)
	}

	got, err = cached.Transcribe(context.Background(), pcm)
	if err != nil || got.Text != "hello there" || got.Confidence != 0.9 {
		t.Fatalf("second Transcribe() = %+v, %v; want the cached result", got, err)
	}
	if n := inner.transcribes.Load(); n != 1 {
		t.Fatalf("inner.transcribes after second (should-be-cached) call = %d, want still 1", n)
	}
}

func TestCachedRecognizerDistinguishesEngineAndAudio(t *testing.T) {
	rdb := requireRedis(t)
	innerA := &countingRecognizer{name: "engine-a", result: Result{Text: "result"}}
	innerB := &countingRecognizer{name: "engine-b", result: Result{Text: "result"}}
	cachedA := NewCached(innerA, rdb, time.Minute)
	cachedB := NewCached(innerB, rdb, time.Minute)

	pcmA := []byte{1, 2, 3}
	pcmB := []byte{4, 5, 6}

	if _, err := cachedA.Transcribe(context.Background(), pcmA); err != nil {
		t.Fatalf("Transcribe() engine-a/A: %v", err)
	}
	if _, err := cachedB.Transcribe(context.Background(), pcmA); err != nil {
		t.Fatalf("Transcribe() engine-b/A: %v", err)
	}
	if _, err := cachedA.Transcribe(context.Background(), pcmB); err != nil {
		t.Fatalf("Transcribe() engine-a/B: %v", err)
	}
	if n := innerA.transcribes.Load(); n != 2 {
		t.Fatalf("innerA.transcribes = %d, want 2 (different audio must not share a cache entry)", n)
	}
	if n := innerB.transcribes.Load(); n != 1 {
		t.Fatalf("innerB.transcribes = %d, want 1 (different engine must not share a cache entry)", n)
	}
}

func TestCachedRecognizerDoesNotCacheAFailedCall(t *testing.T) {
	rdb := requireRedis(t)
	inner := &countingRecognizer{name: "whisper", err: errors.New("engine down")}
	cached := NewCached(inner, rdb, time.Minute)

	pcm := []byte{9, 9, 9}

	if _, err := cached.Transcribe(context.Background(), pcm); err == nil {
		t.Fatalf("Transcribe() error = nil, want the upstream error")
	}
	if _, err := cached.Transcribe(context.Background(), pcm); err == nil {
		t.Fatalf("second Transcribe() error = nil, want the upstream error again")
	}
	if n := inner.transcribes.Load(); n != 2 {
		t.Fatalf("inner.transcribes = %d, want 2 (a failed call must not be cached)", n)
	}
}

func TestCachedRecognizerFallsBackWhenRedisUnavailable(t *testing.T) {
	inner := &countingRecognizer{name: "whisper", result: Result{Text: "hello"}}
	// Nothing listens here; Redis calls fail fast rather than hanging.
	deadRdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", DialTimeout: 200 * time.Millisecond, MaxRetries: -1})
	cached := NewCached(inner, deadRdb, time.Minute)

	got, err := cached.Transcribe(context.Background(), []byte{1})
	if err != nil || got.Text != "hello" {
		t.Fatalf("Transcribe() with Redis down = %+v, %v; want direct call to still succeed", got, err)
	}
}
