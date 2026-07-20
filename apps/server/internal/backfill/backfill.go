// Package backfill asynchronously fills in the native-language translation
// for chat turns that never got one — e.g. turns saved before the
// translation feature existed, or ones whose original async translation
// call failed. It never runs inline with a learner's live conversation:
// sessions are queued when viewed (see httpserver.sessionDetailHandler) and
// drained by a single background Worker, so translation never competes with
// or delays the interactive pipeline. See Queue and Worker.
package backfill

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"buddy/server/internal/llm"
	"buddy/server/internal/pipeline"
	"buddy/server/internal/store"
)

const (
	queueKey      = "buddy:translate:queue"      // Redis list: pending job JSON, FIFO
	queuedSet     = "buddy:translate:queued"     // Redis set: dedupe key of jobs queued or in flight
	processingKey = "buddy:translate:processing" // Redis sorted set: job JSON -> claimed-at unix time
	lockKey       = "buddy:translate:lock"       // Redis mutex: at most one drain running cluster-wide

	// lockTTL is a safety ceiling, not the normal release path: Worker
	// explicitly deletes the lock as soon as a drain pass ends (queue
	// empty, or a Redis error), so under normal operation it's held only
	// for as long as translation is actually running — the moment the
	// LLM has spare capacity again, the lock is free. If a process dies
	// mid-drain without releasing it, this bounds how long the stuck
	// lock can wedge every other replica out of the queue.
	lockTTL = time.Hour

	// processingStaleThreshold bounds how long a job may sit claimed in
	// processingKey before reapStaleJobs treats its worker as dead and
	// puts it back on queueKey. Translating one session is normally a
	// handful of LLM calls (seconds), so this is generous headroom for a
	// slow pass, not a tight budget — it only exists to recover from a
	// replica that died mid-drainOnce (killed, OOM, deploy) between
	// claiming the job and finishing it, which otherwise left the job's
	// dedupeKey stuck in queuedSet forever (see reapStaleJobs).
	processingStaleThreshold = 10 * time.Minute

	// pollInterval is how often an idle Worker checks whether it should
	// try to acquire the lock and drain — background work, so this
	// trades a little latency on newly-queued sessions for not hammering
	// Redis with a tight loop.
	pollInterval = 15 * time.Second
)

// job is one session queued for translation backfill.
type job struct {
	UserID    string `json:"userId"`
	SessionID string `json:"sessionId"`
}

func (j job) dedupeKey() string { return j.UserID + ":" + j.SessionID }

// Queue enqueues sessions that have at least one turn missing its
// translation. Backed by Redis so every server replica shares one queue and
// one dedupe set. A nil *Queue (Redis not configured — see
// config.Config.RedisClusterHost) makes Enqueue a no-op, the same
// "optional feature, falls through to doing nothing" convention as
// internal/identity's Redis-backed OIDC cache.
type Queue struct {
	rdb redis.UniversalClient
}

func NewQueue(rdb redis.UniversalClient) *Queue {
	return &Queue{rdb: rdb}
}

// Enqueue adds (userID, sessionID) to the backfill queue, unless it's
// already queued or currently being processed — SAdd's return value doubles
// as that check (0 added means the member already existed). Best-effort and
// asynchronous by design: a Redis error just leaves this session's
// translations missing until its next view retries, the same as any other
// optional-cache failure in this codebase; callers should invoke this via
// `go` so a slow/unavailable Redis never delays the response that triggered
// it (see httpserver.sessionDetailHandler).
func (q *Queue) Enqueue(ctx context.Context, userID, sessionID string) {
	if q == nil {
		return
	}
	j := job{UserID: userID, SessionID: sessionID}
	added, err := q.rdb.SAdd(ctx, queuedSet, j.dedupeKey()).Result()
	if err != nil {
		log.Printf("backfill: enqueue check %s: %v", j.dedupeKey(), err)
		return
	}
	if added == 0 {
		return // already queued or currently being processed
	}
	b, err := json.Marshal(j)
	if err != nil {
		log.Printf("backfill: marshal job %s: %v", j.dedupeKey(), err)
		q.rdb.SRem(context.Background(), queuedSet, j.dedupeKey())
		return
	}
	if err := q.rdb.RPush(ctx, queueKey, b).Err(); err != nil {
		log.Printf("backfill: enqueue push %s: %v", j.dedupeKey(), err)
		q.rdb.SRem(context.Background(), queuedSet, j.dedupeKey())
	}
}

// Worker drains Queue, translating every turn missing a translation in each
// queued session — one session, one turn at a time — serialized across the
// whole cluster by a Redis lock (lockKey), so however many server replicas
// are running, only one ever calls the translation LLM at once.
type Worker struct {
	rdb   redis.UniversalClient
	store store.Store
	pipe  *pipeline.Pipeline
}

func NewWorker(rdb redis.UniversalClient, st store.Store, pipe *pipeline.Pipeline) *Worker {
	return &Worker{rdb: rdb, store: st, pipe: pipe}
}

// Run polls for work every pollInterval until ctx is canceled. Start it with
// `go worker.Run(ctx)`; it never blocks its caller.
func (w *Worker) Run(ctx context.Context) {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.drainOnce(ctx)
		}
	}
}

// drainOnce processes every currently-queued session, but only if this
// replica wins the cluster-wide lock — otherwise another replica is already
// draining, and this tick is a no-op. The lock is released the moment the
// queue empties (or a Redis error ends the pass early), not held for the
// full lockTTL, so the next poll — here or on another replica — can pick up
// newly-queued work immediately; lockTTL only bounds a crashed drain.
//
// Each job is moved into processingKey the instant it's dequeued, and out
// again only once fully handled (translateSession finished + queuedSet
// cleared) — see reapStaleJobs for why: it's what lets a job survive this
// process dying mid-job instead of leaving its session stuck forever.
func (w *Worker) drainOnce(ctx context.Context) {
	token := strconv.FormatInt(time.Now().UnixNano(), 10)
	acquired, err := w.rdb.SetNX(ctx, lockKey, token, lockTTL).Result()
	if err != nil || !acquired {
		return
	}
	defer w.rdb.Del(context.Background(), lockKey)

	w.reapStaleJobs(ctx)

	for {
		raw, err := w.rdb.LPop(ctx, queueKey).Result()
		if errors.Is(err, redis.Nil) {
			return // queue drained
		}
		if err != nil {
			log.Printf("backfill: dequeue: %v", err)
			return
		}
		var j job
		if err := json.Unmarshal([]byte(raw), &j); err != nil {
			log.Printf("backfill: bad queue entry %q: %v", raw, err)
			continue
		}
		if err := w.rdb.ZAdd(ctx, processingKey, redis.Z{Score: float64(time.Now().Unix()), Member: raw}).Err(); err != nil {
			log.Printf("backfill: claim %s: %v", j.dedupeKey(), err)
		}
		w.translateSession(ctx, j.UserID, j.SessionID)
		if err := w.rdb.SRem(context.Background(), queuedSet, j.dedupeKey()).Err(); err != nil {
			log.Printf("backfill: dequeue mark %s: %v", j.dedupeKey(), err)
		}
		if err := w.rdb.ZRem(context.Background(), processingKey, raw).Err(); err != nil {
			log.Printf("backfill: release claim %s: %v", j.dedupeKey(), err)
		}
	}
}

// reapStaleJobs re-queues any job that's been sitting in processingKey
// longer than processingStaleThreshold — the signature of a replica that
// claimed it (LPop + ZAdd in drainOnce) and then died before finishing
// (SRem + ZRem), which previously left the job's dedupeKey stuck in
// queuedSet forever: every future Enqueue for that session would see SAdd
// return 0 ("already queued") and silently no-op, so the session's missing
// translations could never be retried again. Runs under the same
// cluster-wide lock drainOnce already holds, so only one replica ever reaps
// at a time. Pushed back onto queueKey (not re-claimed here) so the normal
// dequeue loop below picks it up like any other pending job.
func (w *Worker) reapStaleJobs(ctx context.Context) {
	cutoff := float64(time.Now().Add(-processingStaleThreshold).Unix())
	stale, err := w.rdb.ZRangeByScore(ctx, processingKey, &redis.ZRangeBy{Min: "-inf", Max: strconv.FormatFloat(cutoff, 'f', 0, 64)}).Result()
	if err != nil {
		log.Printf("backfill: reap: scan: %v", err)
		return
	}
	for _, raw := range stale {
		if err := w.rdb.RPush(ctx, queueKey, raw).Err(); err != nil {
			log.Printf("backfill: reap: requeue: %v", err)
			continue
		}
		if err := w.rdb.ZRem(ctx, processingKey, raw).Err(); err != nil {
			log.Printf("backfill: reap: clear claim: %v", err)
		}
	}
}

// translateSession fills in every turn in (userID, sessionID) missing a
// translation, feeding each one every turn before it (verbatim, original
// text, translated or not) as context via pipeline.Pipeline.TranslateWithContext
// — the same "conversation so far" shape correct() uses live — so a
// re-translated turn reads the same as if it had been translated the moment
// it was said. Best-effort per turn: one failed or empty translation is
// logged and skipped, not retried within this pass — the turn is simply
// picked up again the next time its session is viewed and re-queued (see
// httpserver.sessionDetailHandler).
func (w *Worker) translateSession(ctx context.Context, userID, sessionID string) {
	_, turns, err := w.store.SessionDetail(ctx, userID, sessionID)
	if err != nil {
		log.Printf("backfill: session detail %s/%s: %v", userID, sessionID, err)
		return
	}

	var priorTurns []llm.Message
	for _, t := range turns {
		text := strings.TrimSpace(t.Text)
		if text == "" {
			continue
		}
		if strings.TrimSpace(t.Translation) == "" {
			translation, err := w.pipe.TranslateWithContext(ctx, priorTurns, text)
			if err != nil {
				log.Printf("backfill: translate %s/%s turn %d/%s: %v", userID, sessionID, t.Turn, t.Role, err)
			} else if translation != "" {
				if err := w.store.SaveTranslation(ctx, userID, sessionID, t.Turn, t.Role, translation); err != nil {
					log.Printf("backfill: save translation %s/%s turn %d/%s: %v", userID, sessionID, t.Turn, t.Role, err)
				}
			}
		}
		priorTurns = append(priorTurns, llm.Message{Role: t.Role, Content: text})
	}
}
