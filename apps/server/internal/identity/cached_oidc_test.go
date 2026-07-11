package identity

import (
	"context"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/coreos/go-oidc/v3/oidc/oidctest"
	"github.com/redis/go-redis/v9"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"
)

// sharedRedis backs every test in this file, started once in TestMain rather
// than per-test (see mysql_test.go's sharedStore for the same reasoning:
// container startup dominates test time, and each test mints its own token
// so cache keys never collide, giving isolation without a fresh container).
var (
	sharedRedis    *redis.Client
	sharedRedisErr error
)

func TestMain(m *testing.M) {
	os.Exit(runIdentityTests(m))
}

func runIdentityTests(m *testing.M) int {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	container, err := tcredis.Run(ctx, "redis:7")
	if err != nil {
		// No/unreachable Docker (sandboxed CI, restricted dev box): record why
		// so container tests skip themselves instead of failing the suite, but
		// still run the pure-function tests (TestBearerToken and friends).
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

// mockToken mints a token signed by priv for the mock Dex at issuer,
// expiring in ttl.
func mockToken(priv *rsa.PrivateKey, issuer, email string, ttl time.Duration) string {
	claims := `{
		"iss": "` + issuer + `",
		"aud": "` + testClientID + `",
		"sub": "` + email + `",
		"exp": ` + strconv.FormatInt(time.Now().Add(ttl).Unix(), 10) + `,
		"email": "` + email + `"
	}`
	return oidctest.SignIDToken(priv, "test-key", oidc.RS256, claims)
}

func bearerRequest(token string) *http.Request {
	req := httptest.NewRequest("GET", "/ws", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	return req
}

// withTiming temporarily overrides the package's cache-timing knobs for a
// test, restoring the originals on cleanup. The production defaults (10m /
// 1m / 30s) are far too slow to exercise directly in a test.
func withTiming(t *testing.T, ttl, trust, lockTTL time.Duration) {
	t.Helper()
	origTTL, origTrust, origLock := cacheTTL, trustWindow, refreshLockTTL
	cacheTTL, trustWindow, refreshLockTTL = ttl, trust, lockTTL
	t.Cleanup(func() {
		cacheTTL, trustWindow, refreshLockTTL = origTTL, origTrust, origLock
	})
}

func TestCachedOIDCIdentifierCachesAcrossCallsWithoutReverifying(t *testing.T) {
	rdb := requireRedis(t)
	withTiming(t, 10*time.Minute, time.Minute, time.Second) // real ttl/trust window: two quick calls stay well inside it

	srv, priv := newMockDex(t)
	inner, err := NewOIDCIdentifier(context.Background(), srv.URL, testClientID)
	if err != nil {
		t.Fatalf("NewOIDCIdentifier: %v", err)
	}
	cached := NewCachedOIDCIdentifier(inner, rdb)

	token := mockToken(priv, srv.URL, "alex@example.com", time.Hour)
	req := bearerRequest(token)

	id, ok := cached.Identify(httptest.NewRecorder(), req)
	if !ok || id != "alex@example.com" {
		t.Fatalf("first Identify() = %q, %v; want alex@example.com, true", id, ok)
	}

	// Kill Dex: a second call that still needed real verification would fail.
	srv.Close()

	id, ok = cached.Identify(httptest.NewRecorder(), req)
	if !ok || id != "alex@example.com" {
		t.Fatalf("cached Identify() after Dex died = %q, %v; want alex@example.com, true (should be served from cache)", id, ok)
	}
}

func TestCachedOIDCIdentifierBackgroundRefreshExtendsTTL(t *testing.T) {
	rdb := requireRedis(t)
	// A trustWindow much shorter than cacheTTL means a hit shortly after
	// caching is already past it and eligible for a background refresh.
	withTiming(t, 2*time.Second, 200*time.Millisecond, 5*time.Second)

	srv, priv := newMockDex(t)
	inner, err := NewOIDCIdentifier(context.Background(), srv.URL, testClientID)
	if err != nil {
		t.Fatalf("NewOIDCIdentifier: %v", err)
	}
	cached := NewCachedOIDCIdentifier(inner, rdb)

	token := mockToken(priv, srv.URL, "casey@example.com", time.Hour)
	req := bearerRequest(token)
	key := cacheKey(token)
	ctx := context.Background()

	if _, ok := cached.Identify(httptest.NewRecorder(), req); !ok {
		t.Fatalf("initial Identify() failed")
	}
	time.Sleep(500 * time.Millisecond) // past trustWindow (200ms); let the TTL visibly decay too

	firstTTL, err := rdb.PTTL(ctx, key).Result()
	if err != nil {
		t.Fatalf("PTTL: %v", err)
	}

	// This hit is past trustWindow, so it kicks off a background
	// revalidation; give it a moment to land.
	if _, ok := cached.Identify(httptest.NewRecorder(), req); !ok {
		t.Fatalf("second Identify() failed")
	}
	time.Sleep(300 * time.Millisecond)

	secondTTL, err := rdb.PTTL(ctx, key).Result()
	if err != nil {
		t.Fatalf("PTTL: %v", err)
	}
	if secondTTL <= firstTTL {
		t.Fatalf("TTL after background refresh = %v, want > %v (refresh should have extended it)", secondTTL, firstTTL)
	}
}

func TestCachedOIDCIdentifierEvictsOnDefinitiveRejectionDuringRefresh(t *testing.T) {
	rdb := requireRedis(t)
	withTiming(t, 10*time.Minute, time.Minute, 5*time.Second) // production-shaped: 1min trust window

	srv, priv := newMockDex(t)
	inner, err := NewOIDCIdentifier(context.Background(), srv.URL, testClientID)
	if err != nil {
		t.Fatalf("NewOIDCIdentifier: %v", err)
	}
	cached := NewCachedOIDCIdentifier(inner, rdb)
	ctx := context.Background()

	// A token that's already expired by the time we verify it — Verify()
	// returns *oidc.TokenExpiredError deterministically, no clock-race
	// needed. We seed the cache entry directly (bypassing save()'s own
	// expiry cap), CachedAt well past trustWindow, so the hit path sees a
	// live-but-past-trust-window entry despite the underlying token being
	// dead: "cache said fresh, Dex says expired now".
	token := mockToken(priv, srv.URL, "dana@example.com", -time.Minute)
	key := cacheKey(token)
	entry := cacheEntry{
		Email:     "dana@example.com",
		CachedAt:  time.Now().Add(-2 * time.Minute),
		ExpiresAt: time.Now().Add(time.Hour),
	}
	b, _ := json.Marshal(entry)
	if err := rdb.Set(ctx, key, b, time.Hour).Err(); err != nil {
		t.Fatalf("seed cache: %v", err)
	}

	req := bearerRequest(token)
	id, ok := cached.Identify(httptest.NewRecorder(), req)
	if !ok || id != "dana@example.com" {
		t.Fatalf("Identify() = %q, %v; want the stale-but-not-yet-evicted cached value", id, ok)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := rdb.Get(ctx, key).Result(); err == redis.Nil {
			return // evicted, as expected
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("cache entry was not evicted after a definitively-expired token surfaced during background refresh")
}

func TestCachedOIDCIdentifierKeepsCacheOnTransientRefreshFailure(t *testing.T) {
	rdb := requireRedis(t)
	withTiming(t, 10*time.Minute, time.Minute, 5*time.Second) // production-shaped: 1min trust window

	srv, priv := newMockDex(t)
	inner, err := NewOIDCIdentifier(context.Background(), srv.URL, testClientID)
	if err != nil {
		t.Fatalf("NewOIDCIdentifier: %v", err)
	}
	cached := NewCachedOIDCIdentifier(inner, rdb)
	ctx := context.Background()

	// inner has never verified anything yet, so its in-process JWKS cache is
	// empty; killing Dex now guarantees any verify attempt has to reach the
	// network for keys and fails.
	srv.Close()

	token := mockToken(priv, srv.URL, "morgan@example.com", time.Hour)
	key := cacheKey(token)
	entry := cacheEntry{
		Email:     "morgan@example.com",
		CachedAt:  time.Now().Add(-2 * time.Minute), // past trustWindow
		ExpiresAt: time.Now().Add(time.Hour),
	}
	b, _ := json.Marshal(entry)
	if err := rdb.Set(ctx, key, b, time.Hour).Err(); err != nil {
		t.Fatalf("seed cache: %v", err)
	}

	req := bearerRequest(token)
	if id, ok := cached.Identify(httptest.NewRecorder(), req); !ok || id != "morgan@example.com" {
		t.Fatalf("Identify() = %q, %v; want the cached value regardless of Dex being down", id, ok)
	}

	time.Sleep(300 * time.Millisecond) // let the background refresh attempt (and fail) run

	got, err := rdb.Get(ctx, key).Result()
	if err != nil {
		t.Fatalf("cache entry should survive a transient refresh failure, got Get error: %v", err)
	}
	var after cacheEntry
	if err := json.Unmarshal([]byte(got), &after); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if after.Email != "morgan@example.com" {
		t.Fatalf("cache entry email = %q, want unchanged morgan@example.com", after.Email)
	}
}

func TestCachedOIDCIdentifierFallsBackWhenRedisUnavailable(t *testing.T) {
	srv, priv := newMockDex(t)
	inner, err := NewOIDCIdentifier(context.Background(), srv.URL, testClientID)
	if err != nil {
		t.Fatalf("NewOIDCIdentifier: %v", err)
	}
	// Nothing listens here; Redis calls fail fast rather than hanging.
	deadRdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", DialTimeout: 200 * time.Millisecond, MaxRetries: -1})
	cached := NewCachedOIDCIdentifier(inner, deadRdb)

	token := mockToken(priv, srv.URL, "riley@example.com", time.Hour)
	req := bearerRequest(token)

	id, ok := cached.Identify(httptest.NewRecorder(), req)
	if !ok || id != "riley@example.com" {
		t.Fatalf("Identify() with Redis down = %q, %v; want direct verification to still succeed", id, ok)
	}
}
