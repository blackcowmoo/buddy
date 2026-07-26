package asyncjob

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// Handler executes one job's work. It must be idempotent: a job may be
// handed to a Handler more than once — most commonly because the Worker
// that first claimed it died mid-handler (crash, redeploy, OOM) and
// reapOnce requeued it once its claim expired, so a second Worker (maybe on
// a different replica) picks it up and reruns it from scratch. There is no
// way to resume a partially-streamed LLM generation, so "from scratch" is
// the deliberate, accepted retry semantics here — Handler implementations
// should check whether their work is already done (e.g. via a durable
// status column) before redoing anything expensive; see
// pipeline.ReplyJobHandler for the pattern.
type Handler func(ctx context.Context, job Job) error

const (
	// claimBlockTimeout bounds each blocking dequeue call so claimLoop
	// periodically wakes up to check ctx.Done() instead of blocking forever
	// past shutdown.
	claimBlockTimeout = 5 * time.Second
	// reapPollInterval is how often reapLoop looks for abandoned claims.
	reapPollInterval = 30 * time.Second
)

// FailureRetryBackoff bounds how long a job whose handler returned an error
// stays claimed before the reaper retries it — deliberately independent of
// claimTTL, which instead bounds how long a *legitimately still-running*
// handler may take before it's presumed crashed (see NewWorker's doc
// comment). Some claimTTLs are loosened to many hours to avoid reaping a
// call that's still legitimately running against a slow local model (e.g.
// transport.CorrectionClaimTTL); without this separate, short backoff, a
// handler that fails fast (bad payload, downstream error) would also wait
// that same multi-hour margin before its retry, instead of recovering soon.
// A var, not a const, so tests can shrink it rather than waiting out the
// production interval.
var FailureRetryBackoff = 2 * time.Minute

// Worker runs one job Kind's handler across concurrency goroutines, each
// independently blocking on Redis to claim the next job. Unlike
// internal/backfill's single cluster-wide-locked drainer, any number of
// Workers — in this process or another replica — can claim and run
// DIFFERENT jobs of the same kind at once: BLMove atomically hands each
// queued job to exactly one caller, with no global lock serializing
// unrelated jobs behind each other. Kinds that do need serialization (e.g.
// translation, to match Pipeline.translationSem's one-at-a-time cap) get
// that by constructing their Worker with concurrency=1, not by
// reintroducing a global lock.
type Worker struct {
	rdb         redis.UniversalClient
	kind        Kind
	handler     Handler
	concurrency int
	claimTTL    time.Duration
}

// NewWorker builds a Worker for kind. concurrency is how many jobs of this
// kind this one Worker (i.e. this one replica) runs at once. claimTTL
// bounds how long a claimed job may run before reapOnce treats its owner as
// dead and puts it back on the queue for someone else to retry — set it
// comfortably above the slowest realistic handler call (e.g. LLM request
// timeout plus margin): too short a TTL reaps and duplicates a job whose
// handler is still legitimately running.
func NewWorker(rdb redis.UniversalClient, kind Kind, concurrency int, claimTTL time.Duration, handler Handler) *Worker {
	if concurrency < 1 {
		concurrency = 1
	}
	return &Worker{rdb: rdb, kind: kind, handler: handler, concurrency: concurrency, claimTTL: claimTTL}
}

// Run blocks until ctx is canceled, running concurrency claim goroutines
// plus one stale-claim reaper. A nil Worker or a nil rdb (Redis not
// configured) makes this an immediate no-op, matching every other
// optional-Redis-feature in this codebase (see cmd/server/main.go).
func (w *Worker) Run(ctx context.Context) {
	if w == nil || w.rdb == nil {
		return
	}
	var wg sync.WaitGroup
	for i := 0; i < w.concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w.claimLoop(ctx)
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		w.reapLoop(ctx)
	}()
	wg.Wait()
}

func (w *Worker) claimLoop(ctx context.Context) {
	for ctx.Err() == nil {
		raw, err := w.rdb.BLMove(ctx, queueKey(w.kind), processingKey(w.kind), "RIGHT", "LEFT", claimBlockTimeout).Result()
		if errors.Is(err, redis.Nil) {
			continue // nothing queued within the timeout; loop to recheck ctx
		}
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("asyncjob: %s: claim: %v", w.kind, err)
			continue
		}
		w.run(raw)
	}
}

// run executes one already-claimed job (raw is its processingKey list
// entry, exactly as claimed). Uses context.Background(), not claimLoop's
// ctx, so the handler isn't cut short by this Worker's own shutdown mid-job
// — see Worker's package-level reasoning about durability across restarts.
func (w *Worker) run(raw string) {
	var job Job
	if err := json.Unmarshal([]byte(raw), &job); err != nil {
		log.Printf("asyncjob: %s: bad job entry, dropping: %v", w.kind, err)
		w.rdb.LRem(context.Background(), processingKey(w.kind), 1, raw)
		return
	}
	ck := claimKey(w.kind, job.ID)
	ok, err := w.rdb.SetNX(context.Background(), ck, "1", w.claimTTL).Result()
	if err != nil {
		log.Printf("asyncjob: %s: claim token %s: %v", w.kind, job.ID, err)
		return
	}
	if !ok {
		// Another worker already holds a live claim on this exact job ID —
		// shouldn't happen (BLMove already gave this goroutine exclusive
		// ownership of the list entry), but guards against running the
		// handler twice concurrently rather than assuming it can't occur.
		return
	}
	if handlerErr := w.handler(context.Background(), job); handlerErr != nil {
		// Deliberately do NOT complete the job here: leave it claimed, but
		// shorten that claim to FailureRetryBackoff (rather than the full,
		// possibly many-hours-long claimTTL) so reapOnce retries it from
		// scratch soon, without hot-looping a handler that's failing fast
		// (e.g. a downstream LLM outage).
		log.Printf("asyncjob: %s: handler failed for job %s: %v", w.kind, job.ID, handlerErr)
		if err := w.rdb.PExpire(context.Background(), ck, FailureRetryBackoff).Err(); err != nil {
			log.Printf("asyncjob: %s: shorten claim after failure %s: %v", w.kind, job.ID, err)
		}
		return
	}
	if err := completeJob(context.Background(), w.rdb, w.kind, job.ID, raw, job.DedupeKey); err != nil {
		log.Printf("asyncjob: %s: complete job %s: %v", w.kind, job.ID, err)
	}
}

func (w *Worker) reapLoop(ctx context.Context) {
	ticker := time.NewTicker(reapPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.reapOnce(context.Background())
		}
	}
}

// reapOnce re-queues any job in processingKey whose claim has expired — the
// signature of a worker that claimed it and then died before completing it
// (crash, OOM, redeploy, or an ungraceful shutdown mid-handler). Safe to
// run concurrently from multiple Workers/replicas: reapOneScript's
// EXISTS-then-move is atomic per job, so at most one reaper wins each stale
// entry.
func (w *Worker) reapOnce(ctx context.Context) {
	raws, err := w.rdb.LRange(ctx, processingKey(w.kind), 0, -1).Result()
	if err != nil {
		log.Printf("asyncjob: %s: reap: scan: %v", w.kind, err)
		return
	}
	for _, raw := range raws {
		var job Job
		if err := json.Unmarshal([]byte(raw), &job); err != nil {
			continue
		}
		job.Attempts++
		requeued, err := json.Marshal(job)
		if err != nil {
			continue
		}
		n, err := reapOneScript.Run(ctx, w.rdb,
			[]string{processingKey(w.kind), claimKey(w.kind, job.ID), queueKey(w.kind)},
			raw, requeued,
		).Int()
		if err != nil {
			log.Printf("asyncjob: %s: reap: requeue %s: %v", w.kind, job.ID, err)
			continue
		}
		if n == 1 {
			log.Printf("asyncjob: %s: reaped abandoned job %s (attempt %d)", w.kind, job.ID, job.Attempts)
		}
	}
}
