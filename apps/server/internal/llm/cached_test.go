package llm

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

// sharedRedis backs every test in this file, started once in TestMain — see
// identity/cached_oidc_test.go's sharedRedis for the same reasoning
// (container startup dominates test time; each test uses distinct model/
// input so cache keys never collide).
var (
	sharedRedis    *redis.Client
	sharedRedisErr error
)

func TestMain(m *testing.M) {
	os.Exit(runLLMTests(m))
}

func runLLMTests(m *testing.M) int {
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

// countingClient counts Complete calls so tests can assert a cache hit never
// reaches the underlying model.
type countingClient struct {
	completes atomic.Int32
	text      string
	err       error
}

func (c *countingClient) ChatStream(ctx context.Context, model string, msgs []Message, onToken func(string)) (string, error) {
	return "", errors.New("not used in these tests")
}

func (c *countingClient) Complete(ctx context.Context, model string, msgs []Message, jsonMode bool) (string, error) {
	c.completes.Add(1)
	return c.text, c.err
}

func TestCachedClientServesSecondCallFromCacheWithoutCallingInner(t *testing.T) {
	rdb := requireRedis(t)
	inner := &countingClient{text: "corrected sentence."}
	cached := NewCached(inner, rdb, time.Minute)

	msgs := []Message{{Role: RoleUser, Content: "check my grammar: I are fine"}}

	got, err := cached.Complete(context.Background(), "model-a", msgs, false)
	if err != nil || got != "corrected sentence." {
		t.Fatalf("first Complete() = %q, %v; want %q, nil", got, err, "corrected sentence.")
	}
	if n := inner.completes.Load(); n != 1 {
		t.Fatalf("inner.completes after first call = %d, want 1", n)
	}

	got, err = cached.Complete(context.Background(), "model-a", msgs, false)
	if err != nil || got != "corrected sentence." {
		t.Fatalf("second Complete() = %q, %v; want %q, nil", got, err, "corrected sentence.")
	}
	if n := inner.completes.Load(); n != 1 {
		t.Fatalf("inner.completes after second (should-be-cached) call = %d, want still 1", n)
	}
}

func TestCachedClientDistinguishesModelAndInput(t *testing.T) {
	rdb := requireRedis(t)
	inner := &countingClient{text: "result"}
	cached := NewCached(inner, rdb, time.Minute)

	msgsA := []Message{{Role: RoleUser, Content: "sentence A"}}
	msgsB := []Message{{Role: RoleUser, Content: "sentence B"}}

	if _, err := cached.Complete(context.Background(), "model-a", msgsA, false); err != nil {
		t.Fatalf("Complete() model-a/A: %v", err)
	}
	if _, err := cached.Complete(context.Background(), "model-b", msgsA, false); err != nil {
		t.Fatalf("Complete() model-b/A: %v", err)
	}
	if _, err := cached.Complete(context.Background(), "model-a", msgsB, false); err != nil {
		t.Fatalf("Complete() model-a/B: %v", err)
	}
	if n := inner.completes.Load(); n != 3 {
		t.Fatalf("inner.completes = %d, want 3 (different model/input must not share a cache entry)", n)
	}
}

func TestCachedClientDoesNotCacheAFailedCall(t *testing.T) {
	rdb := requireRedis(t)
	inner := &countingClient{err: errors.New("upstream down")}
	cached := NewCached(inner, rdb, time.Minute)

	msgs := []Message{{Role: RoleUser, Content: "retry me"}}

	if _, err := cached.Complete(context.Background(), "model-a", msgs, false); err == nil {
		t.Fatalf("Complete() error = nil, want the upstream error")
	}
	if _, err := cached.Complete(context.Background(), "model-a", msgs, false); err == nil {
		t.Fatalf("second Complete() error = nil, want the upstream error again")
	}
	if n := inner.completes.Load(); n != 2 {
		t.Fatalf("inner.completes = %d, want 2 (a failed call must not be cached)", n)
	}
}

func TestCachedClientFallsBackWhenRedisUnavailable(t *testing.T) {
	inner := &countingClient{text: "result"}
	// Nothing listens here; Redis calls fail fast rather than hanging.
	deadRdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", DialTimeout: 200 * time.Millisecond, MaxRetries: -1})
	cached := NewCached(inner, deadRdb, time.Minute)

	msgs := []Message{{Role: RoleUser, Content: "hello"}}
	got, err := cached.Complete(context.Background(), "model-a", msgs, false)
	if err != nil || got != "result" {
		t.Fatalf("Complete() with Redis down = %q, %v; want direct call to still succeed", got, err)
	}
}
