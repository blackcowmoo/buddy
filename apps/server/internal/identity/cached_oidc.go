package identity

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/redis/go-redis/v9"
)

// Cache-timing knobs. var, not const, so tests can shrink them (the
// production defaults are far too slow to exercise directly in a test).
var (
	// cacheTTL is the longest a verified token's result is trusted without
	// being rechecked — capped per-entry by the token's own "exp" claim
	// (see save), so this can never make a genuinely expired token look
	// valid. It's also the hard ceiling an entry survives to when
	// revalidation keeps failing (or nothing asks for it): once an entry
	// turns cacheTTL old, load() stops treating it as a hit regardless of
	// how refresh attempts went.
	cacheTTL = 10 * time.Minute

	// trustWindow is how long after caching an entry is served with zero
	// extra work — no refresh attempted, just the cached answer. Once an
	// entry is older than this, every cache hit becomes eligible to kick
	// off a background revalidation (still throttled by refreshLockTTL);
	// the requesting goroutine gets today's cached answer immediately
	// either way — refresh happens after the response goes out, invisibly
	// to the caller.
	trustWindow = 1 * time.Minute

	// refreshLockTTL bounds a single background revalidation and doubles
	// as its throttle: it's a Redis lock, so only one revalidation for a
	// given token can run at a time, and a fresh attempt can't start again
	// until this lock expires.
	refreshLockTTL = 30 * time.Second

	// revalidateTimeout bounds how long a background revalidation is
	// allowed to wait on Dex before giving up for this round.
	revalidateTimeout = 5 * time.Second
)

// CachedOIDCIdentifier wraps OIDCIdentifier with a Redis-backed cache of
// verification results, keyed by a hash of the raw bearer token.
//
// Verifying a Dex-issued JWT is already a local, in-process check once the
// JWKS is cached (see oidc.go) — Dex itself is only re-contacted when an
// unrecognized key ID shows up (key rotation). So this cache isn't chasing
// network round trips; it exists so that WS-handshake/API auth stays fast
// and available even if Dex is slow or briefly down, without ever serving a
// token past its own "exp".
//
// Lifecycle of one cached entry, relative to when it was (re)verified:
//   - [0, trustWindow): served as-is, no extra work at all.
//   - [trustWindow, cacheTTL): still served immediately, but each hit is
//     eligible to trigger a throttled background revalidation (see
//     refreshInBackground). Success resets the entry back to age 0 with a
//     fresh cacheTTL; a definitive rejection (token actually invalid now)
//     evicts it immediately; a transient failure (or simply no requests
//     arriving) leaves it untouched, so it just keeps being served until it
//     ages out at cacheTTL.
//   - >= cacheTTL: load() stops treating it as a hit; the next request
//     verifies synchronously, like a first-ever request for that token.
//
// Redis is purely an optimization layer, never a hard dependency: any Redis
// error (down, timeout, whatever) makes this fall through to verifying the
// token directly via inner, exactly as if caching were disabled.
// rdb is redis.UniversalClient rather than a concrete type so this works
// unchanged against a standalone *redis.Client (e.g. in tests) or a
// *redis.ClusterClient (production) — every call this file makes (Get, Set,
// SetNX, Del) is single-key, so cluster slot routing is transparent either
// way.
type CachedOIDCIdentifier struct {
	inner OIDCIdentifier
	rdb   redis.UniversalClient
}

func NewCachedOIDCIdentifier(inner OIDCIdentifier, rdb redis.UniversalClient) CachedOIDCIdentifier {
	return CachedOIDCIdentifier{inner: inner, rdb: rdb}
}

type cacheEntry struct {
	Email     string    `json:"email"`
	CachedAt  time.Time `json:"cachedAt"`
	ExpiresAt time.Time `json:"expiresAt"` // min(cachedAt+cacheTTL, token's own exp)
}

func (c CachedOIDCIdentifier) Identify(w http.ResponseWriter, r *http.Request) (string, bool) {
	raw, ok := bearerToken(r)
	if !ok {
		return "", false
	}
	key := cacheKey(raw)

	if entry, hit := c.load(r.Context(), key); hit {
		if time.Since(entry.CachedAt) >= trustWindow {
			c.refreshInBackground(key, raw)
		}
		return entry.Email, true
	}

	// Hard miss — never cached, Redis unavailable, or past ExpiresAt: verify
	// synchronously so a request is never handed an identity we haven't
	// actually checked.
	email, expiry, err := c.inner.verify(r.Context(), raw)
	if err != nil {
		return "", false
	}
	c.save(context.Background(), key, email, expiry)
	return email, true
}

func (c CachedOIDCIdentifier) load(ctx context.Context, key string) (cacheEntry, bool) {
	raw, err := c.rdb.Get(ctx, key).Bytes()
	if err != nil {
		return cacheEntry{}, false // miss, or Redis is unhappy — either way, fall through
	}
	var entry cacheEntry
	if err := json.Unmarshal(raw, &entry); err != nil {
		return cacheEntry{}, false
	}
	if !time.Now().Before(entry.ExpiresAt) {
		return cacheEntry{}, false
	}
	return entry, true
}

func (c CachedOIDCIdentifier) save(ctx context.Context, key, email string, tokenExpiry time.Time) {
	now := time.Now()
	expiresAt := now.Add(cacheTTL)
	if tokenExpiry.Before(expiresAt) {
		expiresAt = tokenExpiry
	}
	ttl := time.Until(expiresAt)
	if ttl <= 0 {
		return // token is already at/past its own exp — nothing worth caching
	}
	b, err := json.Marshal(cacheEntry{Email: email, CachedAt: now, ExpiresAt: expiresAt})
	if err != nil {
		return
	}
	if err := c.rdb.Set(ctx, key, b, ttl).Err(); err != nil {
		log.Printf("identity: redis cache write: %v", err)
	}
}

// refreshInBackground revalidates a cache entry that's past trustWindow
// without making the current request wait for it: the caller already has
// today's cached answer, this just keeps the next request fast too. The
// SETNX lock both serializes revalidation (only one goroutine per token does
// the real Dex/JWKS check) and throttles it (refreshLockTTL: at most one
// attempt per token per that interval) — so a token that's past trustWindow
// gets rechecked roughly every refreshLockTTL for as long as requests keep
// arriving, not on every single request.
func (c CachedOIDCIdentifier) refreshInBackground(key, raw string) {
	lockCtx, lockCancel := context.WithTimeout(context.Background(), revalidateTimeout)
	acquired, err := c.rdb.SetNX(lockCtx, key+":refresh", "1", refreshLockTTL).Result()
	lockCancel()
	if err != nil || !acquired {
		return
	}

	go func() {
		vctx, cancel := context.WithTimeout(context.Background(), revalidateTimeout)
		defer cancel()

		email, expiry, verr := c.inner.verify(vctx, raw)
		switch {
		case verr == nil:
			c.save(context.Background(), key, email, expiry)
		case isTransientVerifyError(vctx, verr):
			// Couldn't reach Dex (or our own timeout fired) — leave the
			// cached entry as-is. It keeps being served as before, and the
			// next request past trustWindow retries once refreshLockTTL
			// releases this lock; worst case it just rides out to its own
			// cacheTTL-based expiry and the next request re-verifies from
			// scratch.
		default:
			// The token itself is bad now (expired, rotated key, wrong
			// issuer/audience) — evict immediately rather than keep
			// serving it for the rest of the already-granted window.
			if err := c.rdb.Del(context.Background(), key).Err(); err != nil {
				log.Printf("identity: redis cache evict: %v", err)
			}
		}
	}()
}

// isTransientVerifyError reports whether verr means buddy failed to reach
// Dex to check the token, as opposed to Dex/JWKS having actually rejected
// it. go-oidc v3 doesn't expose a distinct type for connectivity failures —
// every remote-key-set error is funneled through one fmt.Errorf("fetching
// keys %w", ...) call (see RemoteKeySet.verify in the go-oidc source), so
// that fixed substring is the most reliable signal available without
// forking the library. vctx.Err() covers revalidateTimeout firing on our
// own bounded context, which is unambiguous either way.
func isTransientVerifyError(vctx context.Context, verr error) bool {
	if vctx.Err() != nil {
		return true
	}
	var expired *oidc.TokenExpiredError
	if errors.As(verr, &expired) {
		return false
	}
	return strings.Contains(verr.Error(), "fetching keys")
}

func cacheKey(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return "oidc:verify:" + hex.EncodeToString(sum[:])
}
