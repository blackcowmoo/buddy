package asyncjob

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"
)

// sharedRedis backs every test in this file, started once in TestMain
// rather than per-test — see internal/backfill/backfill_test.go for the
// same reasoning (container startup dominates test time; each test cleans
// its own kind's keys so isolation doesn't need a fresh container).
var (
	sharedRedis    *redis.Client
	sharedRedisErr error
)

func TestMain(m *testing.M) {
	os.Exit(runTests(m))
}

func runTests(m *testing.M) int {
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

// testKind returns a Kind unique to the calling test, so tests can run in
// parallel (or just not worry about cleaning up after each other) without
// colliding on the same Redis keys.
func testKind(t *testing.T) Kind {
	t.Helper()
	return Kind(t.Name())
}

func requireRedis(t *testing.T) *redis.Client {
	t.Helper()
	if sharedRedisErr != nil {
		t.Skipf("redis testcontainer unavailable (no/unreachable Docker?): %v", sharedRedisErr)
	}
	return sharedRedis
}

// ---- Queue.Enqueue -----------------------------------------------------

func TestEnqueueDedupesInFlight(t *testing.T) {
	rdb := requireRedis(t)
	kind := testKind(t)
	q := NewQueue(rdb)
	ctx := context.Background()

	j1, ok1, err := q.Enqueue(ctx, kind, "dedupe-1", map[string]string{"a": "1"})
	if err != nil || !ok1 {
		t.Fatalf("first enqueue: job=%+v ok=%v err=%v", j1, ok1, err)
	}
	_, ok2, err := q.Enqueue(ctx, kind, "dedupe-1", map[string]string{"a": "2"})
	if err != nil {
		t.Fatalf("second enqueue: %v", err)
	}
	if ok2 {
		t.Fatalf("second enqueue with same dedupeKey should have been deduped")
	}

	n, err := rdb.LLen(ctx, queueKey(kind)).Result()
	if err != nil {
		t.Fatalf("LLen: %v", err)
	}
	if n != 1 {
		t.Fatalf("queue length = %d, want 1", n)
	}
}

func TestEnqueueAllowsDistinctDedupeKeys(t *testing.T) {
	rdb := requireRedis(t)
	kind := testKind(t)
	q := NewQueue(rdb)
	ctx := context.Background()

	for _, key := range []string{"a", "b", "c"} {
		if _, ok, err := q.Enqueue(ctx, kind, key, map[string]string{"k": key}); err != nil || !ok {
			t.Fatalf("enqueue %s: ok=%v err=%v", key, ok, err)
		}
	}

	n, err := rdb.LLen(ctx, queueKey(kind)).Result()
	if err != nil {
		t.Fatalf("LLen: %v", err)
	}
	if n != 3 {
		t.Fatalf("queue length = %d, want 3", n)
	}
}

func TestEnqueueNilQueueIsNoop(t *testing.T) {
	var q *Queue
	job, ok, err := q.Enqueue(context.Background(), KindReply, "x", 1) // must not panic
	if err != nil || ok || job.ID != "" {
		t.Fatalf("nil queue Enqueue should be a harmless no-op, got job=%+v ok=%v err=%v", job, ok, err)
	}
}

// ---- Worker: concurrent claiming ---------------------------------------

func TestClaimConcurrentWorkersGetDistinctJobs(t *testing.T) {
	rdb := requireRedis(t)
	kind := testKind(t)
	q := NewQueue(rdb)
	ctx := context.Background()

	const n = 20
	for i := 0; i < n; i++ {
		if _, ok, err := q.Enqueue(ctx, kind, string(rune('a'+i)), i); err != nil || !ok {
			t.Fatalf("enqueue %d: ok=%v err=%v", i, ok, err)
		}
	}

	var mu sync.Mutex
	seen := map[string]int{}
	var wg sync.WaitGroup
	handler := func(_ context.Context, job Job) error {
		mu.Lock()
		seen[job.ID]++
		mu.Unlock()
		return nil
	}

	runCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	const workers = 5
	for i := 0; i < workers; i++ {
		w := NewWorker(rdb, kind, 2, time.Minute, handler)
		wg.Add(1)
		go func() {
			defer wg.Done()
			w.Run(runCtx)
		}()
	}

	deadline := time.After(8 * time.Second)
	for {
		mu.Lock()
		count := len(seen)
		mu.Unlock()
		if count == n {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("only %d/%d jobs handled within deadline", count, n)
		case <-time.After(50 * time.Millisecond):
		}
	}
	cancel()
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if len(seen) != n {
		t.Fatalf("handled %d distinct jobs, want %d", len(seen), n)
	}
	for id, count := range seen {
		if count != 1 {
			t.Fatalf("job %s handled %d times, want exactly 1 (duplicate delivery)", id, count)
		}
	}
}

// ---- Worker: completion + idempotency ----------------------------------

func TestCompleteClearsProcessingClaimAndDedupe(t *testing.T) {
	rdb := requireRedis(t)
	kind := testKind(t)
	q := NewQueue(rdb)
	ctx := context.Background()

	if _, ok, err := q.Enqueue(ctx, kind, "d1", 1); err != nil || !ok {
		t.Fatalf("enqueue: ok=%v err=%v", ok, err)
	}

	var calls int
	w := NewWorker(rdb, kind, 1, time.Minute, func(_ context.Context, job Job) error {
		calls++
		return nil
	})
	runCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() { w.Run(runCtx); close(done) }()

	waitForCondition(t, 3*time.Second, func() bool { return calls == 1 })
	cancel()
	<-done

	if n, _ := rdb.LLen(ctx, queueKey(kind)).Result(); n != 0 {
		t.Fatalf("queue should be empty, length = %d", n)
	}
	if n, _ := rdb.LLen(ctx, processingKey(kind)).Result(); n != 0 {
		t.Fatalf("processing list should be empty, length = %d", n)
	}
	if n, _ := rdb.SCard(ctx, dedupeSetKey(kind)).Result(); n != 0 {
		t.Fatalf("dedupe set should be empty, size = %d", n)
	}

	// Re-enqueueing the same dedupeKey after completion must succeed (not
	// treated as still in flight).
	if _, ok, err := q.Enqueue(ctx, kind, "d1", 2); err != nil || !ok {
		t.Fatalf("re-enqueue after completion: ok=%v err=%v", ok, err)
	}
}

// ---- Worker: handler failure leaves job for reap -----------------------

func TestHandlerErrorLeavesJobForReap(t *testing.T) {
	rdb := requireRedis(t)
	kind := testKind(t)
	q := NewQueue(rdb)
	ctx := context.Background()

	job, ok, err := q.Enqueue(ctx, kind, "d1", 1)
	if err != nil || !ok {
		t.Fatalf("enqueue: ok=%v err=%v", ok, err)
	}

	var calls int
	w := NewWorker(rdb, kind, 1, time.Minute, func(_ context.Context, j Job) error {
		calls++
		return errFake
	})
	runCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() { w.Run(runCtx); close(done) }()

	waitForCondition(t, 2*time.Second, func() bool { return calls == 1 })
	cancel()
	<-done

	// Job stays claimed (not completed, not immediately requeued) —
	// visible in processingKey with its claim key still present.
	if n, _ := rdb.LLen(ctx, processingKey(kind)).Result(); n != 1 {
		t.Fatalf("processing list should still hold the failed job, length = %d", n)
	}
	exists, err := rdb.Exists(ctx, claimKey(kind, job.ID)).Result()
	if err != nil {
		t.Fatalf("Exists: %v", err)
	}
	if exists != 1 {
		t.Fatalf("claim key should still exist after a handler failure")
	}
	if n, _ := rdb.LLen(ctx, queueKey(kind)).Result(); n != 0 {
		t.Fatalf("failed job must not be immediately requeued, queue length = %d", n)
	}
}

var errFake = fakeErr("handler failed")

type fakeErr string

func (e fakeErr) Error() string { return string(e) }

// TestHandlerErrorShortensClaimToBackoffNotClaimTTL guards the fix for a
// real incident: claimTTL was loosened to 25h (see
// transport.CorrectionClaimTTL) so a legitimately-slow local-model call
// never gets reaped mid-flight, but that same 25h TTL used to be the ONLY
// thing that requeued a job whose handler failed fast — so a genuine
// grammar-check failure sat unretried for 25 hours despite the UI promising
// an automatic retry. FailureRetryBackoff exists precisely to decouple the
// two: a failed handler's claim should expire (and get reaped) on its own
// short schedule, independent of how long claimTTL tolerates a still-running
// call. This drives reapOnce directly (as the other reap tests do) rather
// than waiting out the real reapLoop ticker.
func TestHandlerErrorShortensClaimToBackoffNotClaimTTL(t *testing.T) {
	rdb := requireRedis(t)
	kind := testKind(t)
	q := NewQueue(rdb)
	ctx := context.Background()

	old := FailureRetryBackoff
	FailureRetryBackoff = 50 * time.Millisecond
	defer func() { FailureRetryBackoff = old }()

	// claimTTL is deliberately huge (as it is in production for slow local
	// models) to prove the requeue comes from FailureRetryBackoff, not from
	// claimTTL ever lapsing.
	job, ok, err := q.Enqueue(ctx, kind, "d1", 1)
	if err != nil || !ok {
		t.Fatalf("enqueue: ok=%v err=%v", ok, err)
	}
	claimed, err := q.TryClaimByID(ctx, job, 25*time.Hour)
	if err != nil || !claimed {
		t.Fatalf("claim: claimed=%v err=%v", claimed, err)
	}
	if err := q.Execute(ctx, job, 25*time.Hour, func(context.Context, Job) error { return errFake }); err == nil {
		t.Fatalf("Execute should surface the handler error")
	}

	// Immediately after the failure, the claim is shortened but not yet
	// expired, so a reap right now must leave it alone.
	w := NewWorker(rdb, kind, 1, 25*time.Hour, nil)
	w.reapOnce(ctx)
	if n, _ := rdb.LLen(ctx, queueKey(kind)).Result(); n != 0 {
		t.Fatalf("queue length = %d, want 0 (backoff not elapsed yet)", n)
	}

	time.Sleep(200 * time.Millisecond) // past FailureRetryBackoff, nowhere near claimTTL
	w.reapOnce(ctx)
	if n, _ := rdb.LLen(ctx, queueKey(kind)).Result(); n != 1 {
		t.Fatalf("queue length = %d, want 1 (job should be requeued once FailureRetryBackoff elapses)", n)
	}
}

func TestFailedJobStopsAfterMaxAttempts(t *testing.T) {
	rdb := requireRedis(t)
	kind := testKind(t)
	q := NewQueue(rdb)
	ctx := context.Background()

	oldBackoff, oldMax := FailureRetryBackoff, MaxAttempts
	FailureRetryBackoff = 10 * time.Millisecond
	MaxAttempts = 2
	t.Cleanup(func() {
		FailureRetryBackoff, MaxAttempts = oldBackoff, oldMax
	})

	job, ok, err := q.Enqueue(ctx, kind, "terminal", 1)
	if err != nil || !ok {
		t.Fatalf("enqueue: ok=%v err=%v", ok, err)
	}
	var calls int
	w := NewWorker(rdb, kind, 1, time.Minute, func(context.Context, Job) error {
		calls++
		return errFake
	})
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { defer close(done); w.Run(runCtx) }()
	defer func() { cancel(); <-done }()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if calls >= 1 {
			time.Sleep(30 * time.Millisecond)
			w.reapOnce(ctx)
		}
		if calls >= 2 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if calls != 2 {
		t.Fatalf("handler calls = %d, want exactly 2", calls)
	}
	if n, _ := rdb.LLen(ctx, queueKey(kind)).Result(); n != 0 {
		t.Fatalf("queue length = %d, want terminal failure removed", n)
	}
	if n, _ := rdb.SIsMember(ctx, dedupeSetKey(kind), job.DedupeKey).Result(); n {
		t.Fatal("terminal failure must release its dedupe key")
	}
}

func TestExecuteRenewsLeaseWhileHandlerRuns(t *testing.T) {
	rdb := requireRedis(t)
	kind := testKind(t)
	q := NewQueue(rdb)
	ctx := context.Background()
	job, ok, err := q.Enqueue(ctx, kind, "renew", 1)
	if err != nil || !ok {
		t.Fatalf("enqueue: ok=%v err=%v", ok, err)
	}
	claimed, err := q.TryClaimByID(ctx, job, time.Second)
	if err != nil || !claimed {
		t.Fatalf("claim: claimed=%v err=%v", claimed, err)
	}

	if err := q.Execute(ctx, job, time.Second, func(context.Context, Job) error {
		time.Sleep(2200 * time.Millisecond)
		if exists, _ := rdb.Exists(ctx, claimKey(kind, job.ID)).Result(); exists != 1 {
			t.Fatal("claim expired while the handler was still running")
		}
		return nil
	}); err != nil {
		t.Fatalf("execute: %v", err)
	}
}

// ---- Reap ---------------------------------------------------------------

func TestReapRequeuesAbandonedClaim(t *testing.T) {
	rdb := requireRedis(t)
	kind := testKind(t)
	ctx := context.Background()

	job := Job{ID: "job-1", Kind: kind, DedupeKey: "d1", Payload: json.RawMessage(`1`)}
	raw, err := json.Marshal(job)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := rdb.RPush(ctx, processingKey(kind), raw).Err(); err != nil {
		t.Fatalf("seed processing: %v", err)
	}
	// No claim key seeded — simulates a claim whose TTL already expired
	// (the owning worker died before it could complete or renew it).

	w := NewWorker(rdb, kind, 1, time.Minute, nil)
	w.reapOnce(ctx)

	if n, _ := rdb.LLen(ctx, processingKey(kind)).Result(); n != 0 {
		t.Fatalf("stale entry should have been removed from processing, length = %d", n)
	}
	requeued, err := rdb.LRange(ctx, queueKey(kind), 0, -1).Result()
	if err != nil {
		t.Fatalf("LRange: %v", err)
	}
	if len(requeued) != 1 {
		t.Fatalf("requeued length = %d, want 1", len(requeued))
	}
	var got Job
	if err := json.Unmarshal([]byte(requeued[0]), &got); err != nil {
		t.Fatalf("unmarshal requeued: %v", err)
	}
	if got.Attempts != 1 {
		t.Fatalf("requeued job Attempts = %d, want 1", got.Attempts)
	}
}

func TestRequeueExpiredFindsDedupedAbandonedJobImmediately(t *testing.T) {
	rdb := requireRedis(t)
	kind := testKind(t)
	q := NewQueue(rdb)
	ctx := context.Background()
	job, ok, err := q.Enqueue(ctx, kind, "expired", 1)
	if err != nil || !ok {
		t.Fatalf("enqueue: ok=%v err=%v", ok, err)
	}
	raw, _ := json.Marshal(job)
	if err := rdb.LRem(ctx, queueKey(kind), 1, raw).Err(); err != nil {
		t.Fatalf("remove queued copy: %v", err)
	}
	if err := rdb.RPush(ctx, processingKey(kind), raw).Err(); err != nil {
		t.Fatalf("seed processing: %v", err)
	}
	moved, err := q.RequeueExpired(ctx, kind, "expired")
	if err != nil || !moved {
		t.Fatalf("RequeueExpired: moved=%v err=%v", moved, err)
	}
	if n, _ := rdb.LLen(ctx, queueKey(kind)).Result(); n != 1 {
		t.Fatalf("queue length = %d, want 1", n)
	}
}

func TestReapLeavesFreshClaimAlone(t *testing.T) {
	rdb := requireRedis(t)
	kind := testKind(t)
	ctx := context.Background()

	job := Job{ID: "job-1", Kind: kind, DedupeKey: "d1", Payload: json.RawMessage(`1`)}
	raw, err := json.Marshal(job)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := rdb.RPush(ctx, processingKey(kind), raw).Err(); err != nil {
		t.Fatalf("seed processing: %v", err)
	}
	if err := rdb.Set(ctx, claimKey(kind, job.ID), "1", time.Minute).Err(); err != nil {
		t.Fatalf("seed claim: %v", err)
	}

	w := NewWorker(rdb, kind, 1, time.Minute, nil)
	w.reapOnce(ctx)

	if n, _ := rdb.LLen(ctx, processingKey(kind)).Result(); n != 1 {
		t.Fatalf("fresh claim should be left alone, processing length = %d", n)
	}
	if n, _ := rdb.LLen(ctx, queueKey(kind)).Result(); n != 0 {
		t.Fatalf("fresh claim must not be requeued, queue length = %d", n)
	}
}

func TestReapDoesNotDuplicateOnRacingReapers(t *testing.T) {
	rdb := requireRedis(t)
	kind := testKind(t)
	ctx := context.Background()

	job := Job{ID: "job-1", Kind: kind, DedupeKey: "d1", Payload: json.RawMessage(`1`)}
	raw, err := json.Marshal(job)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := rdb.RPush(ctx, processingKey(kind), raw).Err(); err != nil {
		t.Fatalf("seed processing: %v", err)
	}

	w1 := NewWorker(rdb, kind, 1, time.Minute, nil)
	w2 := NewWorker(rdb, kind, 1, time.Minute, nil)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); w1.reapOnce(ctx) }()
	go func() { defer wg.Done(); w2.reapOnce(ctx) }()
	wg.Wait()

	n, err := rdb.LLen(ctx, queueKey(kind)).Result()
	if err != nil {
		t.Fatalf("LLen: %v", err)
	}
	if n != 1 {
		t.Fatalf("queue length after racing reapers = %d, want exactly 1 (no duplicate requeue)", n)
	}
}

// ---- Fast path: TryClaimByID --------------------------------------------

func TestTryClaimByIDWinsBeforeAWorkerPolls(t *testing.T) {
	rdb := requireRedis(t)
	kind := testKind(t)
	q := NewQueue(rdb)
	ctx := context.Background()

	job, ok, err := q.Enqueue(ctx, kind, "d1", 1)
	if err != nil || !ok {
		t.Fatalf("enqueue: ok=%v err=%v", ok, err)
	}

	claimed, err := q.TryClaimByID(ctx, job, time.Minute)
	if err != nil {
		t.Fatalf("TryClaimByID: %v", err)
	}
	if !claimed {
		t.Fatalf("inline claim should have won: nothing else was competing for this job")
	}
	if n, _ := rdb.LLen(ctx, queueKey(kind)).Result(); n != 0 {
		t.Fatalf("job should be gone from the queue after inline claim, length = %d", n)
	}
	if n, _ := rdb.LLen(ctx, processingKey(kind)).Result(); n != 1 {
		t.Fatalf("job should be moved into processing, length = %d", n)
	}
}

func TestTryClaimByIDLosesRaceToAWorker(t *testing.T) {
	rdb := requireRedis(t)
	kind := testKind(t)
	q := NewQueue(rdb)
	ctx := context.Background()

	job, ok, err := q.Enqueue(ctx, kind, "d1", 1)
	if err != nil || !ok {
		t.Fatalf("enqueue: ok=%v err=%v", ok, err)
	}

	// Simulate a pooled worker claiming it first via the normal path.
	var calls int
	done := make(chan struct{})
	w := NewWorker(rdb, kind, 1, time.Minute, func(_ context.Context, j Job) error {
		calls++
		close(done)
		return nil
	})
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go w.Run(runCtx)
	<-done
	cancel()

	claimed, err := q.TryClaimByID(ctx, job, time.Minute)
	if err != nil {
		t.Fatalf("TryClaimByID: %v", err)
	}
	if claimed {
		t.Fatalf("inline claim should have lost: the worker already completed this job")
	}
	if calls != 1 {
		t.Fatalf("worker handler calls = %d, want 1", calls)
	}
}

// ---- Fast path: EnqueueAndTryRun ----------------------------------------

func TestEnqueueAndTryRunRunsHandlerInlineAndCompletes(t *testing.T) {
	rdb := requireRedis(t)
	kind := testKind(t)
	q := NewQueue(rdb)
	ctx := context.Background()

	var calls int
	ran, err := q.EnqueueAndTryRun(ctx, kind, "d1", "user/session#1", 1, time.Minute, func(_ context.Context, j Job) error {
		calls++
		return nil
	})
	if err != nil {
		t.Fatalf("EnqueueAndTryRun: %v", err)
	}
	if !ran {
		t.Fatalf("ran = false, want true: nothing else was competing for this job")
	}
	if calls != 1 {
		t.Fatalf("handler calls = %d, want 1", calls)
	}
	// Success must leave the job fully completed, exactly as Execute does:
	// out of processing, claim released, dedupe entry cleared.
	if n, _ := rdb.LLen(ctx, processingKey(kind)).Result(); n != 0 {
		t.Fatalf("processing length = %d, want 0 after a successful inline run", n)
	}
	if n, _ := rdb.SCard(ctx, dedupeSetKey(kind)).Result(); n != 0 {
		t.Fatalf("dedupe set size = %d, want 0 after a successful inline run", n)
	}
}

func TestEnqueueAndTryRunReportsNotRunWhenDeduped(t *testing.T) {
	rdb := requireRedis(t)
	kind := testKind(t)
	q := NewQueue(rdb)
	ctx := context.Background()

	// An in-flight job for the same dedupe key: the second call must dedupe
	// against it rather than running a second handler for the same work.
	if _, ok, err := q.Enqueue(ctx, kind, "d1", 1); err != nil || !ok {
		t.Fatalf("seed enqueue: ok=%v err=%v", ok, err)
	}

	var calls int
	ran, err := q.EnqueueAndTryRun(ctx, kind, "d1", "user/session#1", 1, time.Minute, func(_ context.Context, j Job) error {
		calls++
		return nil
	})
	if err != nil {
		t.Fatalf("EnqueueAndTryRun: %v", err)
	}
	// ran=false with a nil error is what tells callers "another attempt owns
	// this job" — the correction hook relies on it to skip its onFailure.
	if ran {
		t.Fatalf("ran = true, want false: an identical job was already queued")
	}
	if calls != 0 {
		t.Fatalf("handler calls = %d, want 0 when deduped", calls)
	}
}

func TestEnqueueAndTryRunReturnsErrorWhenHandlerFails(t *testing.T) {
	rdb := requireRedis(t)
	kind := testKind(t)
	q := NewQueue(rdb)
	ctx := context.Background()

	wantErr := errors.New("handler boom")
	ran, err := q.EnqueueAndTryRun(ctx, kind, "d1", "user/session#1", 1, time.Minute, func(_ context.Context, j Job) error {
		return wantErr
	})
	// ran=true with a non-nil error is the "this caller ran it and it
	// failed" case: reply falls back to polling and correction fires
	// onFailure off exactly this signal.
	if !ran {
		t.Fatalf("ran = false, want true: the handler did run, it just failed")
	}
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want it to wrap %v", err, wantErr)
	}
	// A failed job stays in processing for the stale-claim reaper to retry.
	if n, _ := rdb.LLen(ctx, processingKey(kind)).Result(); n != 1 {
		t.Fatalf("processing length = %d, want 1 (left for the reaper) after a failed inline run", n)
	}
}

func TestEnqueueAndTryRunOnNilQueueIsNoOp(t *testing.T) {
	var q *Queue
	var calls int
	ran, err := q.EnqueueAndTryRun(context.Background(), Kind("nil-queue"), "d1", "user/session#1", 1, time.Minute, func(_ context.Context, j Job) error {
		calls++
		return nil
	})
	if err != nil {
		t.Fatalf("EnqueueAndTryRun on nil queue: %v", err)
	}
	if ran || calls != 0 {
		t.Fatalf("ran=%v calls=%d, want false/0: a nil Queue is a safe no-op", ran, calls)
	}
}

func TestEnqueueAndRunInBackgroundSurvivesRequestCancellation(t *testing.T) {
	rdb := requireRedis(t)
	kind := testKind(t)
	q := NewQueue(rdb)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan struct{})
	if err := q.EnqueueAndRunInBackground(ctx, kind, "cancelled-request", "test", 1, time.Minute, func(context.Context, Job) error {
		close(done)
		return nil
	}); err != nil {
		t.Fatalf("EnqueueAndRunInBackground: %v", err)
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("job was not executed after the request context was cancelled")
	}
}

// ---- test helpers ---------------------------------------------------------

func waitForCondition(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !cond() {
		t.Fatalf("condition not met within %s", timeout)
	}
}
