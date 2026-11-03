package quizcache

import (
	"context"
	"errors"
	"net"
	"os"
	"testing"
	"time"

	"buddy/server/internal/testdocker"

	"github.com/redis/go-redis/v9"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"
)

var (
	sharedRedis    *redis.Client
	sharedRedisErr error
)

func TestMain(m *testing.M) {
	os.Exit(runCacheTests(m))
}

func runCacheTests(m *testing.M) int {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	container, err := tcredis.Run(ctx, "redis:7", testdocker.WithProcessSession())
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

func TestKeyNormalizesTheGradedTriple(t *testing.T) {
	base := Key("He ___ to school.", "goes", "Walks")
	if other := Key(" he ___ to school. ", " Goes ", "walks"); other != base {
		t.Fatalf("case/whitespace variants split the key: %q vs %q", base, other)
	}
	for _, variant := range []struct{ prompt, answer, learner string }{
		{"He ___ to school.", "goes", "Jogs"},
		{"He ___ home.", "goes", "Walks"},
		{"He ___ to school.", "went", "Walks"},
	} {
		if got := Key(variant.prompt, variant.answer, variant.learner); got == base {
			t.Fatalf("distinct triple %q collided with %q", variant, base)
		}
	}
}

func TestSaveLookupRenewTTL(t *testing.T) {
	rdb := requireRedis(t)
	ctx := context.Background()
	store := New(rdb, time.Hour)
	key := Key("He ___ home.", "went", "goed")
	t.Cleanup(func() { _ = rdb.Del(context.Background(), key).Err() })

	if _, found := store.Lookup(ctx, key); found {
		t.Fatal("Lookup found an unsaved key")
	}
	if err := store.Save(ctx, key, Verdict{Correct: false, Similar: false}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, found := store.Lookup(ctx, key)
	if !found || got.Correct || got.Similar {
		t.Fatalf("Lookup = %+v, found=%v; want the saved false verdict", got, found)
	}
	if ttl := rdb.PTTL(ctx, key).Val(); ttl <= 0 || ttl > time.Hour {
		t.Fatalf("PTTL after Save = %v; want (0, 1h]", ttl)
	}

	// A legacy/external write without a TTL must gain one on Renew — that is
	// how a wrong verdict re-hit keeps its expiry instead of living forever.
	if err := rdb.Set(ctx, key, `{"correct":false,"similar":false}`, 0).Err(); err != nil {
		t.Fatalf("seed without TTL: %v", err)
	}
	if ttl := rdb.PTTL(ctx, key).Val(); ttl != -1 {
		t.Fatalf("PTTL of seeded key = %v; want -1 (no expiry)", ttl)
	}
	store.Renew(ctx, key)
	if ttl := rdb.PTTL(ctx, key).Val(); ttl <= 0 || ttl > time.Hour {
		t.Fatalf("PTTL after Renew = %v; want (0, 1h]", ttl)
	}

	// Renew must not create (or resurrect) keys: a miss re-checked via Chat
	// must not turn into a stuck entry with a TTL and no verdict.
	missing := Key("She ___ away.", "ran", "flew")
	store.Renew(ctx, missing)
	if err := rdb.Get(ctx, missing).Err(); !errors.Is(err, redis.Nil) {
		t.Fatalf("Renew created a missing key (get err = %v)", err)
	}
}

func TestLookupIgnoresMalformedEntries(t *testing.T) {
	rdb := requireRedis(t)
	ctx := context.Background()
	store := New(rdb, time.Hour)
	key := Key("It is ___.", "red", "crimson")
	t.Cleanup(func() { _ = rdb.Del(context.Background(), key).Err() })

	if err := rdb.Set(ctx, key, `not json`, time.Hour).Err(); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, found := store.Lookup(ctx, key); found {
		t.Fatal("Lookup served a malformed entry")
	}
}

func TestDegradedRedisIsAMiss(t *testing.T) {
	// A client whose dial always fails models a downed/unreachable Redis: the
	// check must fall through to Chat, never surface the Redis error.
	rdb := redis.NewClient(&redis.Options{
		Addr:        "127.0.0.1:1",
		MaxRetries:  -1,
		DialTimeout: 100 * time.Millisecond,
		Dialer: func(context.Context, string, string) (net.Conn, error) {
			return nil, errors.New("dial blocked by test")
		},
	})
	t.Cleanup(func() { _ = rdb.Close() })
	store := New(rdb, time.Second)
	if _, found := store.Lookup(context.Background(), "any-key"); found {
		t.Fatal("Lookup on a dead Redis reported a hit")
	}
	if err := store.Save(context.Background(), "any-key", Verdict{Correct: true, Similar: true}); err == nil {
		t.Fatal("Save on a dead Redis reported success")
	}
	store.Renew(context.Background(), "any-key") // must not panic
}

func TestNilStoreIsDisabled(t *testing.T) {
	var store *Cache
	if _, found := store.Lookup(context.Background(), "k"); found {
		t.Fatal("nil store Lookup reported a hit")
	}
	if err := store.Save(context.Background(), "k", Verdict{}); err != nil {
		t.Fatalf("nil store Save: %v", err)
	}
	store.Renew(context.Background(), "k") // must not panic
	if New(nil, time.Hour) != nil {
		t.Fatal("New(nil) must be a nil (disabled) store")
	}
}
