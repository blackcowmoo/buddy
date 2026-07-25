package asyncjob

import (
	"context"
	"encoding/json"
	"fmt"
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
// claim released, dedupe entry cleared); on error, it's left claimed so
// the stale-claim reaper retries it from scratch once its claim expires,
// exactly as if a Worker's own handler had failed. This is what lets a
// caller run a job inline (e.g. transport's fast path, streaming tokens
// straight to a connection that's still open) with the same durability
// guarantees as the background Worker pool.
func (q *Queue) Execute(ctx context.Context, job Job, handler Handler) error {
	if q == nil {
		return nil
	}
	if err := handler(ctx, job); err != nil {
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
