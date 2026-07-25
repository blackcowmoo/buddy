package httpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"

	"buddy/server/internal/backfill"
	"buddy/server/internal/protocol"
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

// awaitQueueLength polls until the named backfill queue reaches want (or
// fails the test after deadline) — sessionDetailHandler enqueues via `go`,
// fired after the response is already written, so tests can't just check
// synchronously.
func awaitQueueLength(t *testing.T, rdb *redis.Client, want int64) {
	t.Helper()
	awaitNamedQueueLength(t, rdb, "buddy:job:{translation}:queue", want)
}

func awaitNamedQueueLength(t *testing.T, rdb *redis.Client, key string, want int64) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		n, err := rdb.LLen(context.Background(), key).Result()
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
	h := sessionDetailHandler(fakeIdentifier{id: "alex", ok: true}, st, q, nil)

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
	h := sessionDetailHandler(fakeIdentifier{id: "alex", ok: true}, st, q, nil)

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
	n, err := rdb.LLen(context.Background(), "buddy:job:{translation}:queue").Result()
	if err != nil {
		t.Fatalf("LLen: %v", err)
	}
	if n != 0 {
		t.Fatalf("queue length = %d, want 0 (every turn already has a translation)", n)
	}
}

// TestSessionDetailEnqueuesCorrectionBackfillWhenAUserTurnHasNoCorrectionStatus
// covers the gap this backfill path exists for: a user turn with no
// Correction and an entirely empty CorrectionStatus (saved before
// correction-job tracking existed, or produced by the no-Redis inline path)
// has no live asyncjob.KindCorrection job for the reaper to retry — nothing
// else will ever pick it back up, so viewing the session must enqueue it.
func TestSessionDetailEnqueuesCorrectionBackfillWhenAUserTurnHasNoCorrectionStatus(t *testing.T) {
	rdb := requireRedis(t)
	q := backfill.NewCorrectionQueue(rdb)

	st := &fakeSessionStore{
		detailMeta: store.SessionMeta{ID: "s1"},
		detailTurns: []store.Turn{
			{Turn: 1, Role: "user", Text: "I has a dog."}, // no Correction, no CorrectionStatus
			{Turn: 1, Role: "assistant", Text: "Nice!"},
		},
	}
	h := sessionDetailHandler(fakeIdentifier{id: "alex", ok: true}, st, nil, q)

	req := httptest.NewRequest("GET", "/api/sessions/s1", nil)
	req.SetPathValue("id", "s1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	awaitNamedQueueLength(t, rdb, "buddy:job:{correction-backfill}:queue", 1)
}

// TestSessionDetailDoesNotEnqueueCorrectionBackfillWhenAlreadyTrackedOrDone
// checks the two cases that must NOT trigger backfill: a turn that already
// has a Correction, and one whose CorrectionStatus is non-empty (tracked by
// a live job whose own reaper already retries it — see
// asyncjob.KindCorrectionBackfill's doc comment).
func TestSessionDetailDoesNotEnqueueCorrectionBackfillWhenAlreadyTrackedOrDone(t *testing.T) {
	rdb := requireRedis(t)
	q := backfill.NewCorrectionQueue(rdb)

	st := &fakeSessionStore{
		detailMeta: store.SessionMeta{ID: "s1"},
		detailTurns: []store.Turn{
			{Turn: 1, Role: "user", Text: "already corrected", Correction: &protocol.Correction{Original: "already corrected", Corrected: "already corrected"}},
			{Turn: 2, Role: "user", Text: "in flight", CorrectionStatus: "pending"},
			{Turn: 3, Role: "user", Text: "errored, reaper owns it", CorrectionStatus: "failed"},
		},
	}
	h := sessionDetailHandler(fakeIdentifier{id: "alex", ok: true}, st, nil, q)

	req := httptest.NewRequest("GET", "/api/sessions/s1", nil)
	req.SetPathValue("id", "s1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	time.Sleep(200 * time.Millisecond)
	n, err := rdb.LLen(context.Background(), "buddy:job:{correction-backfill}:queue").Result()
	if err != nil {
		t.Fatalf("LLen: %v", err)
	}
	if n != 0 {
		t.Fatalf("queue length = %d, want 0 (no turn is untracked)", n)
	}
}

func TestSessionDetailWithNilCorrectionQueueDoesNotPanic(t *testing.T) {
	st := &fakeSessionStore{
		detailMeta: store.SessionMeta{ID: "s1"},
		detailTurns: []store.Turn{
			{Turn: 1, Role: "user", Text: "hello"}, // no correction status, but queue is nil (Redis unconfigured)
		},
	}
	h := sessionDetailHandler(fakeIdentifier{id: "alex", ok: true}, st, nil, nil)

	req := httptest.NewRequest("GET", "/api/sessions/s1", nil)
	req.SetPathValue("id", "s1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req) // must not panic

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

func TestSessionDetailWithNilTranslateQueueDoesNotPanic(t *testing.T) {
	st := &fakeSessionStore{
		detailMeta: store.SessionMeta{ID: "s1"},
		detailTurns: []store.Turn{
			{Turn: 1, Role: "user", Text: "hello"}, // missing translation, but queue is nil (Redis unconfigured)
		},
	}
	h := sessionDetailHandler(fakeIdentifier{id: "alex", ok: true}, st, nil, nil)

	req := httptest.NewRequest("GET", "/api/sessions/s1", nil)
	req.SetPathValue("id", "s1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req) // must not panic

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

func TestSessionDetailUnauthorizedWhenIdentifyFails(t *testing.T) {
	h := sessionDetailHandler(fakeIdentifier{ok: false}, &fakeSessionStore{}, nil, nil)

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
	h := sessionDetailHandler(fakeIdentifier{id: "alex", ok: true}, st, nil, nil)

	req := httptest.NewRequest("GET", "/api/sessions/s1", nil)
	req.SetPathValue("id", "s1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

// TestSessionDetailDefaultsLimitWhenQueryParamsAreAbsent documents that a
// plain GET (no ?before=/?limit=, what the frontend's very first page load
// sends) still asks the store for a bounded page — defaultSessionPageLimit —
// rather than silently falling back to "everything", which is the behavior
// this handler had before pagination existed.
func TestSessionDetailDefaultsLimitWhenQueryParamsAreAbsent(t *testing.T) {
	st := &fakeSessionStore{
		detailMeta:  store.SessionMeta{ID: "s1"},
		detailTurns: []store.Turn{{Turn: 1, Role: "user", Text: "hi", Translation: "안녕"}},
	}
	h := sessionDetailHandler(fakeIdentifier{id: "alex", ok: true}, st, nil, nil)

	req := httptest.NewRequest("GET", "/api/sessions/s1", nil)
	req.SetPathValue("id", "s1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if st.detailBeforeTurn != 0 || st.detailLimit != defaultSessionPageLimit {
		t.Fatalf("SessionDetail called with (before=%d, limit=%d), want (0, %d)", st.detailBeforeTurn, st.detailLimit, defaultSessionPageLimit)
	}
}

// TestSessionDetailForwardsBeforeAndLimitQueryParams documents that
// ?before=&?limit= (sent when the frontend scrolls up for an older page)
// pass straight through to the store as the turn cursor and page size.
func TestSessionDetailForwardsBeforeAndLimitQueryParams(t *testing.T) {
	st := &fakeSessionStore{
		detailMeta:  store.SessionMeta{ID: "s1"},
		detailTurns: []store.Turn{{Turn: 1, Role: "user", Text: "hi", Translation: "안녕"}},
	}
	h := sessionDetailHandler(fakeIdentifier{id: "alex", ok: true}, st, nil, nil)

	req := httptest.NewRequest("GET", "/api/sessions/s1?before=42&limit=10", nil)
	req.SetPathValue("id", "s1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if st.detailBeforeTurn != 42 || st.detailLimit != 10 {
		t.Fatalf("SessionDetail called with (before=%d, limit=%d), want (42, 10)", st.detailBeforeTurn, st.detailLimit)
	}
}

// TestSessionDetailExplicitZeroLimitRequestsWholeTranscript documents that
// ?limit=0 (what pollMissingFeedback in apps/web/src/App.tsx sends) is
// deliberately different from omitting ?limit= altogether — it routes to
// the unbounded store.SessionDetail instead of the paginated
// SessionDetailPage, rather than being defaulted to defaultSessionPageLimit
// like an absent or unparseable value.
func TestSessionDetailExplicitZeroLimitRequestsWholeTranscript(t *testing.T) {
	st := &fakeSessionStore{
		detailMeta:  store.SessionMeta{ID: "s1"},
		detailTurns: []store.Turn{{Turn: 1, Role: "user", Text: "hi", Translation: "안녕"}},
	}
	h := sessionDetailHandler(fakeIdentifier{id: "alex", ok: true}, st, nil, nil)

	req := httptest.NewRequest("GET", "/api/sessions/s1?limit=0", nil)
	req.SetPathValue("id", "s1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !st.detailUnboundedCalled {
		t.Fatalf("SessionDetailPage was called, want the unbounded SessionDetail")
	}
}

// TestSessionDetailResponseIncludesHasMore checks the JSON response surfaces
// the store's hasMore flag — the frontend's cue to offer/attempt loading
// another page when the learner scrolls to the top of the transcript.
func TestSessionDetailResponseIncludesHasMore(t *testing.T) {
	st := &fakeSessionStore{
		detailMeta:    store.SessionMeta{ID: "s1"},
		detailTurns:   []store.Turn{{Turn: 5, Role: "user", Text: "hi", Translation: "안녕"}},
		detailHasMore: true,
	}
	h := sessionDetailHandler(fakeIdentifier{id: "alex", ok: true}, st, nil, nil)

	req := httptest.NewRequest("GET", "/api/sessions/s1", nil)
	req.SetPathValue("id", "s1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	var body struct {
		HasMore bool `json:"hasMore"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !body.HasMore {
		t.Fatalf("response hasMore = false, want true")
	}
}
