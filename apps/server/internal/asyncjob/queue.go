package asyncjob

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// Redis key helpers. Every key for a given kind shares the "{kind}" hash
// tag, so multi-key Lua scripts (and BLMove, which requires source and
// destination to live on the same cluster slot) work correctly under Redis
// Cluster — see buildRedis in cmd/server/main.go, which always constructs a
// ClusterClient.
func queueKey(kind Kind) string      { return fmt.Sprintf("buddy:job:{%s}:queue", kind) }
func processingKey(kind Kind) string { return fmt.Sprintf("buddy:job:{%s}:processing", kind) }
func dedupeSetKey(kind Kind) string  { return fmt.Sprintf("buddy:job:{%s}:dedupe", kind) }
func claimKey(kind Kind, jobID string) string {
	return fmt.Sprintf("buddy:job:{%s}:claim:%s", kind, jobID)
}

// enqueueScript adds a job unless its dedupe key is already queued or in
// flight, atomically (so two concurrent Enqueue calls for the same
// dedupeKey can't both succeed).
var enqueueScript = redis.NewScript(`
if redis.call('SISMEMBER', KEYS[1], ARGV[1]) == 1 then
	return 0
end
redis.call('SADD', KEYS[1], ARGV[1])
redis.call('RPUSH', KEYS[2], ARGV[2])
return 1
`)

// claimByIDScript removes a specific job straight out of its queue (rather
// than via the normal blocking dequeue) and marks it claimed, atomically. If
// a Worker's BLMove already claimed it first, LREM finds nothing and this
// is a no-op — see Queue.TryClaimByID.
var claimByIDScript = redis.NewScript(`
local removed = redis.call('LREM', KEYS[1], 1, ARGV[1])
if removed == 0 then
	return 0
end
redis.call('RPUSH', KEYS[2], ARGV[1])
redis.call('SET', KEYS[3], '1', 'PX', ARGV[2])
return 1
`)

// completeScript marks a job permanently done: drop it from processing,
// release its claim, and clear its dedupe entry so the same dedupeKey can
// be enqueued again in the future.
var completeScript = redis.NewScript(`
redis.call('LREM', KEYS[1], 1, ARGV[1])
redis.call('DEL', KEYS[2])
redis.call('SREM', KEYS[3], ARGV[2])
return 1
`)

// reapOneScript moves one stale processing entry back onto its queue,
// atomically checked against its claim key so two reapers racing the same
// entry can't both requeue it (only one's EXISTS-then-LREM sees it still
// present).
var reapOneScript = redis.NewScript(`
if redis.call('EXISTS', KEYS[2]) == 1 then
	return 0
end
local removed = redis.call('LREM', KEYS[1], 1, ARGV[1])
if removed == 1 then
	redis.call('RPUSH', KEYS[3], ARGV[2])
end
return removed
`)

// Queue enqueues durable, cluster-shared background jobs onto Redis — one
// FIFO list per Kind. A nil *Queue makes every method a safe no-op, the
// same "optional feature, falls through to doing nothing" convention as
// internal/backfill.Queue and internal/identity's Redis-backed OIDC cache
// (Redis is disabled by default; see config.Config.RedisClusterHost).
type Queue struct {
	rdb redis.UniversalClient
}

func NewQueue(rdb redis.UniversalClient) *Queue {
	return &Queue{rdb: rdb}
}

// Enqueue adds a job of the given kind unless dedupeKey is already queued
// or in flight. ok is false (zero Job) when it was deduped. On success, the
// returned Job can be passed to TryClaimByID immediately — see that
// method's doc comment for why that matters.
func (q *Queue) Enqueue(ctx context.Context, kind Kind, dedupeKey string, payload any) (job Job, ok bool, err error) {
	if q == nil {
		return Job{}, false, nil
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return Job{}, false, fmt.Errorf("asyncjob: marshal payload: %w", err)
	}
	job = Job{
		ID:         uuid.New().String(),
		Kind:       kind,
		DedupeKey:  dedupeKey,
		Payload:    body,
		EnqueuedAt: time.Now().Unix(),
	}
	raw, err := json.Marshal(job)
	if err != nil {
		return Job{}, false, fmt.Errorf("asyncjob: marshal job: %w", err)
	}
	added, err := enqueueScript.Run(ctx, q.rdb, []string{dedupeSetKey(kind), queueKey(kind)}, dedupeKey, raw).Int()
	if err != nil {
		return Job{}, false, fmt.Errorf("asyncjob: enqueue: %w", err)
	}
	if added == 0 {
		return Job{}, false, nil // already queued or in flight
	}
	return job, true, nil
}

// RequeueExpired moves an abandoned processing entry back to its queue when
// its claim has already expired. This is useful on a new enqueue attempt:
// the dedupe key intentionally survives reaping, so the new request must not
// create a duplicate job, but it should be able to kick an expired one
// immediately instead of waiting for reapLoop's next 30-second tick.
func (q *Queue) RequeueExpired(ctx context.Context, kind Kind, dedupeKey string) (bool, error) {
	if q == nil {
		return false, nil
	}
	raws, err := q.rdb.LRange(ctx, processingKey(kind), 0, -1).Result()
	if err != nil {
		return false, fmt.Errorf("asyncjob: inspect expired job: %w", err)
	}
	for _, raw := range raws {
		var job Job
		if err := json.Unmarshal([]byte(raw), &job); err != nil || job.DedupeKey != dedupeKey {
			continue
		}
		job.Attempts++
		requeued, err := json.Marshal(job)
		if err != nil {
			return false, fmt.Errorf("asyncjob: marshal expired job: %w", err)
		}
		n, err := reapOneScript.Run(ctx, q.rdb,
			[]string{processingKey(kind), claimKey(kind, job.ID), queueKey(kind)},
			raw, requeued,
		).Int()
		if err != nil {
			return false, fmt.Errorf("asyncjob: requeue expired job: %w", err)
		}
		return n == 1, nil
	}
	return false, nil
}

// TryClaimByID attempts to claim job (just returned by Enqueue) directly
// out of its queue, before any pooled Worker's blocking dequeue gets to it.
// This is what lets the connection/request that created a job run it
// inline instead of waiting on the worker pool — e.g. transport.Handler
// claims its own just-enqueued reply job so it can keep streaming tokens
// over the socket that's still open, with identical latency to calling the
// LLM directly. It stays correct if it loses the race: ok is false if a
// Worker already claimed this exact job (LREM finds nothing), and the
// caller must then rely on the worker pool (and, for a live connection,
// polling) to eventually complete it.
func (q *Queue) TryClaimByID(ctx context.Context, job Job, claimTTL time.Duration) (ok bool, err error) {
	if q == nil {
		return false, nil
	}
	raw, err := json.Marshal(job)
	if err != nil {
		return false, fmt.Errorf("asyncjob: marshal job: %w", err)
	}
	claimed, err := claimByIDScript.Run(ctx, q.rdb,
		[]string{queueKey(job.Kind), processingKey(job.Kind), claimKey(job.Kind, job.ID)},
		raw, claimTTL.Milliseconds(),
	).Int()
	if err != nil {
		return false, fmt.Errorf("asyncjob: claim by id: %w", err)
	}
	return claimed == 1, nil
}

// Execute runs handler for job, which the caller must have already
// durably claimed (e.g. via TryClaimByID) — applying the same
// completion/leave-for-reap semantics as a pooled Worker (see
// Worker.run): on success, job is marked done (removed from processing,
// claim released, dedupe entry cleared); on error, its claim is shortened
// to FailureRetryBackoff so the stale-claim reaper retries it from scratch
// soon, exactly as if a Worker's own handler had failed. This is what lets a
// caller run a job inline (e.g. transport's fast path, streaming tokens
// straight to a connection that's still open) with the same durability
// guarantees as the background Worker pool.
func (q *Queue) Execute(ctx context.Context, job Job, claimTTL time.Duration, handler Handler) error {
	if q == nil {
		return nil
	}
	if err := runWithLease(ctx, q.rdb, job.Kind, job.ID, claimTTL, func(handlerCtx context.Context) error {
		return handler(handlerCtx, job)
	}); err != nil {
		if job.Attempts+1 >= MaxAttempts {
			raw, marshalErr := json.Marshal(job)
			if marshalErr == nil {
				if abandonErr := abandonJob(ctx, q.rdb, job.Kind, job.ID, raw, job.DedupeKey); abandonErr != nil {
					log.Printf("asyncjob: %s: abandon job %s: %v", job.Kind, job.ID, abandonErr)
				}
			}
			return err
		}
		if expireErr := q.rdb.PExpire(ctx, claimKey(job.Kind, job.ID), FailureRetryBackoff).Err(); expireErr != nil {
			log.Printf("asyncjob: %s: shorten claim after failure %s: %v", job.Kind, job.ID, expireErr)
		}
		return err
	}
	raw, err := json.Marshal(job)
	if err != nil {
		return fmt.Errorf("asyncjob: marshal job: %w", err)
	}
	if err := completeJob(ctx, q.rdb, job.Kind, job.ID, raw, job.DedupeKey); err != nil {
		return fmt.Errorf("asyncjob: complete: %w", err)
	}
	return nil
}

// EnqueueAndTryRun is the shared "enqueue, then try to run it inline" fast
// path behind every transport job hook (reply/correction/translation/title
// in package transport): Enqueue, and — only if this call is the one that
// created the job rather than deduping against one already in flight —
// TryClaimByID and Execute it before any pooled Worker's blocking dequeue
// gets to it. Enqueue/claim/execute errors are logged here, tagged with
// kind and logID (e.g. "userID/sessionID#turn"), so call sites don't each
// repeat the same three log lines. ran reports whether this call actually
// invoked handler; false covers both "deduped" and "lost the claim race to
// a pooled Worker" — cases callers already treat identically, since another
// attempt owns reporting that job's outcome.
func (q *Queue) EnqueueAndTryRun(ctx context.Context, kind Kind, dedupeKey, logID string, payload any, claimTTL time.Duration, handler Handler) (ran bool, err error) {
	if q == nil {
		return false, nil
	}
	job, ok, err := q.Enqueue(ctx, kind, dedupeKey, payload)
	if err != nil {
		log.Printf("asyncjob: %s: enqueue %s: %v", kind, logID, err)
		return false, err
	}
	if !ok {
		return false, nil // deduped: another attempt already owns this job
	}
	claimed, err := q.TryClaimByID(ctx, job, claimTTL)
	if err != nil {
		log.Printf("asyncjob: %s: inline claim %s: %v", kind, logID, err)
		return false, err
	}
	if !claimed {
		return false, nil // lost the race to a pooled Worker
	}
	if err := q.Execute(ctx, job, claimTTL, handler); err != nil {
		log.Printf("asyncjob: %s: inline execute %s: %v", kind, logID, err)
		return true, err
	}
	return true, nil
}

// EnqueueAndRunInBackground is EnqueueAndTryRun's split-context sibling for
// callers that must return before the job finishes but still want it to run
// immediately rather than wait on the pooled Worker: Enqueue happens
// synchronously on ctx (cheap — no LLM call — so it's safe to await before
// e.g. an HTTP response), while TryClaimByID/Execute run in a detached
// goroutine on context.Background() so a request ending doesn't cut the job
// short. Used by transport.EnqueueStudySummaryJob/EnqueueStudyQuizJob, whose
// httpserver.sessionEndHandler caller needs to respond as soon as the job is
// durably queued. Enqueue/claim/execute errors are logged here, tagged with
// kind and logID, the same as EnqueueAndTryRun.
func (q *Queue) EnqueueAndRunInBackground(ctx context.Context, kind Kind, dedupeKey, logID string, payload any, claimTTL time.Duration, handler Handler) error {
	if q == nil {
		return nil
	}
	// The database mutation that created this job may already be committed
	// when the HTTP client disconnects. Do not let that request cancellation
	// strand the durable job between "pending" in the database and Redis.
	// Enqueue is cheap and bounded by Redis' own client timeouts; the actual
	// handler is detached below and never uses the request context either.
	enqueueCtx := context.WithoutCancel(ctx)
	job, ok, err := q.Enqueue(enqueueCtx, kind, dedupeKey, payload)
	if err != nil {
		return fmt.Errorf("asyncjob: %s: enqueue %s: %w", kind, logID, err)
	}
	if !ok {
		// A deduped job can still be an abandoned processing entry whose claim
		// expired just before this request arrived. Requeue it now; the normal
		// worker will execute the same durable job, without creating a duplicate.
		if _, err := q.RequeueExpired(enqueueCtx, kind, dedupeKey); err != nil {
			log.Printf("asyncjob: %s: retry expired %s: %v", kind, logID, err)
		}
		return nil
	}
	go func() {
		claimed, err := q.TryClaimByID(context.Background(), job, claimTTL)
		if err != nil {
			log.Printf("asyncjob: %s: background claim %s: %v", kind, logID, err)
			return
		}
		if !claimed {
			return // lost the race to a pooled Worker, which owns it now
		}
		if err := q.Execute(context.Background(), job, claimTTL, handler); err != nil {
			log.Printf("asyncjob: %s: background execute %s: %v", kind, logID, err)
		}
	}()
	return nil
}

// EnqueueOrRunInline is the shared "durable queue if Redis is configured,
// otherwise a detached best-effort goroutine" fallback behind every caller
// that wants a job to keep running after its own request/connection ends
// without requiring Redis to do it (httpserver.sessionEndHandler,
// sessionRestudyHandler, and transport.FinalizeSession, which both of those
// — plus CorrectionJobHandler's own instant-session auto-finalize — funnel
// through): when queue is non-nil, enqueue runs synchronously on ctx (cheap
// — no LLM call — the queue's own EnqueueAndRunInBackground handles
// backgrounding the actual work); otherwise inline runs the whole job
// itself, so it must be backgrounded here on a detached context.Background()
// goroutine to get the same "outlives this response" behavior without
// Redis.
func EnqueueOrRunInline(queue *Queue, ctx context.Context, enqueueErrLabel string, enqueue func(ctx context.Context) error, inlineErrLabel string, inline func(ctx context.Context) error) {
	if queue != nil {
		if err := enqueue(ctx); err != nil {
			log.Printf("%s: %v", enqueueErrLabel, err)
		}
		return
	}
	go func() {
		if err := inline(context.Background()); err != nil {
			log.Printf("%s: %v", inlineErrLabel, err)
		}
	}()
}

// completeJob marks a claimed job done: removed from processing, its claim
// released, and its dedupe entry cleared, so a later Enqueue with the same
// dedupe key is treated as a fresh job rather than a duplicate of one
// that's already finished. raw is the job's still-queued list entry
// (whatever bytes/string form the caller already has on hand — Execute's
// freshly marshaled JSON, or Worker.run's raw BLMove result) that
// completeScript needs to LREM out of processingKey.
func completeJob(ctx context.Context, rdb redis.UniversalClient, kind Kind, id string, raw any, dedupeKey string) error {
	return completeScript.Run(ctx, rdb,
		[]string{processingKey(kind), claimKey(kind, id), dedupeSetKey(kind)}, raw, dedupeKey,
	).Err()
}

// abandonJob permanently removes a failed job and releases its dedupe key.
// It intentionally uses the same atomic cleanup as successful completion so
// a later user action can enqueue a fresh attempt.
func abandonJob(ctx context.Context, rdb redis.UniversalClient, kind Kind, id string, raw any, dedupeKey string) error {
	return completeJob(ctx, rdb, kind, id, raw, dedupeKey)
}
