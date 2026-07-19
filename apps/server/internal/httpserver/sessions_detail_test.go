package httpserver

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"

	"buddy/server/internal/backfill"
	"buddy/server/internal/store"
)

// sharedRedis backs every test in this file, started once in TestMain rather
// than per-test — see internal/identity/cached_oidc_test.go for the same
// reasoning (container startup dominates test time).
var (
	sharedRedis    *redis.Client
	sharedRedisErr error
)

func TestMain(m *testing.M) {
	os.Exit(runHTTPServerTests(m))
}

func runHTTPServerTests(m *testing.M) int {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	container, err := tcredis.Run(ctx, "redis:7")
	if err != nil {
		sharedRedisErr = err // no/unreachable Docker: skip container-backed tests below
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
	if err := sharedRedis.FlushAll(context.Background()).Err(); err != nil {
		t.Fatalf("flush redis state: %v", err)
	}
	return sharedRedis
}

// awaitQueueLength polls until the backfill queue reaches want (or fails the
// test after deadline) — sessionDetailHandler enqueues via `go`, fired after
// the response is already written, so tests can't just check synchronously.
func awaitQueueLength(t *testing.T, rdb *redis.Client, want int64) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		n, err := rdb.LLen(context.Background(), "buddy:translate:queue").Result()
		if err != nil {
			t.Fatalf("LLen: %v", err)
		}
		if n == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("queue length = %d after deadline, want %d", n, want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestSessionDetailEnqueuesBackfillWhenATurnIsMissingTranslation(t *testing.T) {
	rdb := requireRedis(t)
	q := backfill.NewQueue(rdb)

	st := &fakeSessionStore{
		detailMeta: store.SessionMeta{ID: "s1"},
		detailTurns: []store.Turn{
			{Turn: 1, Role: "user", Text: "hello", Translation: "안녕하세요"},
			{Turn: 1, Role: "assistant", Text: "hi there"}, // missing translation
		},
	}
	h := sessionDetailHandler(fakeIdentifier{id: "alex", ok: true}, st, q)

	req := httptest.NewRequest("GET", "/api/sessions/s1", nil)
	req.SetPathValue("id", "s1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	awaitQueueLength(t, rdb, 1)
}

func TestSessionDetailDoesNotEnqueueWhenEveryTurnIsTranslated(t *testing.T) {
	rdb := requireRedis(t)
	q := backfill.NewQueue(rdb)

	st := &fakeSessionStore{
		detailMeta: store.SessionMeta{ID: "s1"},
		detailTurns: []store.Turn{
			{Turn: 1, Role: "user", Text: "hello", Translation: "안녕하세요"},
			{Turn: 1, Role: "assistant", Text: "hi there", Translation: "안녕하세요!"},
		},
	}
	h := sessionDetailHandler(fakeIdentifier{id: "alex", ok: true}, st, q)

	req := httptest.NewRequest("GET", "/api/sessions/s1", nil)
	req.SetPathValue("id", "s1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	// Nothing should show up even after waiting past the async goroutine's
	// normal completion time.
	time.Sleep(200 * time.Millisecond)
	n, err := rdb.LLen(context.Background(), "buddy:translate:queue").Result()
	if err != nil {
		t.Fatalf("LLen: %v", err)
	}
	if n != 0 {
		t.Fatalf("queue length = %d, want 0 (every turn already has a translation)", n)
	}
}

func TestSessionDetailWithNilTranslateQueueDoesNotPanic(t *testing.T) {
	st := &fakeSessionStore{
		detailMeta: store.SessionMeta{ID: "s1"},
		detailTurns: []store.Turn{
			{Turn: 1, Role: "user", Text: "hello"}, // missing translation, but queue is nil (Redis unconfigured)
		},
	}
	h := sessionDetailHandler(fakeIdentifier{id: "alex", ok: true}, st, nil)

	req := httptest.NewRequest("GET", "/api/sessions/s1", nil)
	req.SetPathValue("id", "s1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req) // must not panic

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

func TestSessionDetailUnauthorizedWhenIdentifyFails(t *testing.T) {
	h := sessionDetailHandler(fakeIdentifier{ok: false}, &fakeSessionStore{}, nil)

	req := httptest.NewRequest("GET", "/api/sessions/s1", nil)
	req.SetPathValue("id", "s1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestSessionDetailNotFoundPropagatesStoreErrNotFound(t *testing.T) {
	st := &fakeSessionStore{detailErr: store.ErrNotFound}
	h := sessionDetailHandler(fakeIdentifier{id: "alex", ok: true}, st, nil)

	req := httptest.NewRequest("GET", "/api/sessions/s1", nil)
	req.SetPathValue("id", "s1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}
