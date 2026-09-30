package asyncjob

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"buddy/server/internal/checkpoint"
	"buddy/server/internal/workguard"

	"github.com/redis/go-redis/v9"
)

func TestCheckpointResumesInlineJobOnAnotherWorker(t *testing.T) {
	rdb := requireRedis(t)
	ctx := context.Background()
	kind := testKind(t)
	q := NewQueue(rdb)
	job := enqueueCheckpointTestJob(t, q, kind)
	claimCheckpointTestJob(t, q, job)
	var firstCalls, secondCalls int
	handler := func(ctx context.Context, _ Job) error {
		first, err := checkpoint.Do(ctx, "first", func() (string, error) {
			firstCalls++
			return "completed first stage", nil
		})
		if err != nil {
			return err
		}
		if first != "completed first stage" {
			t.Fatalf("first stage = %q", first)
		}
		_, err = checkpoint.Do(ctx, "second", func() (string, error) {
			secondCalls++
			if secondCalls == 1 {
				return "partial response", errFake
			}
			return "completed second stage", nil
		})
		return err
	}
	if err := q.Execute(ctx, job, time.Minute, handler); !errors.Is(err, errFake) {
		t.Fatalf("first attempt = %v, want interrupted second stage", err)
	}
	checkpoints, err := rdb.HGetAll(ctx, checkpointKey(kind, job.ID)).Result()
	if err != nil {
		t.Fatal(err)
	}
	if len(checkpoints) != 1 || checkpoints["first"] != "completed first stage" {
		t.Fatalf("checkpoints = %v, want only completed first stage", checkpoints)
	}
	if ttl, err := rdb.TTL(ctx, checkpointKey(kind, job.ID)).Result(); err != nil || ttl != -1 {
		t.Fatalf("checkpoint TTL = %v, %v; want no expiry while pending", ttl, err)
	}

	// Removing the claim deterministically represents lease expiry after a
	// process exit, without depending on elapsed time or a heartbeat race.
	if err := rdb.Del(ctx, claimKey(kind, job.ID)).Err(); err != nil {
		t.Fatal(err)
	}
	otherClient := redis.NewClient(rdb.Options())
	t.Cleanup(func() { _ = otherClient.Close() })
	worker := NewWorker(otherClient, kind, 1, time.Minute, handler)
	worker.reapOnce(ctx)
	runCheckpointTestWorker(t, worker)
	if firstCalls != 1 || secondCalls != 2 {
		t.Fatalf("generation calls = (%d, %d), want (1, 2)", firstCalls, secondCalls)
	}
	assertCheckpointJobCleaned(t, rdb, job)
}

func TestCheckpointOwnershipFencesReadsWritesAndCleanup(t *testing.T) {
	rdb := requireRedis(t)
	ctx := context.Background()
	kind := testKind(t)
	q := NewQueue(rdb)
	job := enqueueCheckpointTestJob(t, q, kind)
	claimCheckpointTestJob(t, q, job)
	oldStore := redisCheckpointStore{rdb: rdb, kind: kind, id: job.ID, token: claimToken(job)}
	if err := oldStore.Save(ctx, "completed", "first successful result"); err != nil {
		t.Fatal(err)
	}
	newToken := claimToken(Job{Attempts: job.Attempts + 1})
	if err := rdb.Set(ctx, claimKey(kind, job.ID), newToken, time.Minute).Err(); err != nil {
		t.Fatal(err)
	}
	newStore := redisCheckpointStore{rdb: rdb, kind: kind, id: job.ID, token: newToken}
	if err := newStore.Save(ctx, "completed", "replacement result"); err != nil {
		t.Fatal(err)
	}
	if result, found, err := newStore.Load(ctx, "completed"); err != nil || !found || result != "first successful result" {
		t.Fatalf("current owner load = (%q, %v, %v), want first successful result", result, found, err)
	}
	if result, found, err := newStore.Load(ctx, "missing"); err != nil || found || result != "" {
		t.Fatalf("missing load = (%q, %v, %v)", result, found, err)
	}
	if result, found, err := oldStore.Load(ctx, "completed"); !errors.Is(err, ErrClaimLost) || found || result != "" {
		t.Fatalf("stale load = (%q, %v, %v), want ErrClaimLost", result, found, err)
	}
	for _, key := range []string{"completed", "late-result"} {
		if err := oldStore.Save(ctx, key, "stale result"); !errors.Is(err, ErrClaimLost) {
			t.Fatalf("stale save %q = %v, want ErrClaimLost", key, err)
		}
		if err := oldStore.Delete(ctx, key); !errors.Is(err, ErrClaimLost) {
			t.Fatalf("stale delete %q = %v, want ErrClaimLost", key, err)
		}
	}
	raw, err := json.Marshal(job)
	if err != nil {
		t.Fatal(err)
	}
	for _, cleanup := range []struct {
		name string
		run  func(context.Context, redis.UniversalClient, Kind, string, any, string, string) error
	}{
		{"complete", completeJob},
		{"abandon", abandonJob},
	} {
		if err := cleanup.run(ctx, rdb, kind, job.ID, raw, job.DedupeKey, claimToken(job)); !errors.Is(err, ErrClaimLost) {
			t.Fatalf("stale %s = %v, want ErrClaimLost", cleanup.name, err)
		}
	}
	if fields := rdb.HGetAll(ctx, checkpointKey(kind, job.ID)).Val(); len(fields) != 1 || fields["completed"] != "first successful result" {
		t.Fatalf("checkpoints changed after stale operations: %v", fields)
	}
	if got := rdb.Get(ctx, claimKey(kind, job.ID)).Val(); got != newToken {
		t.Fatalf("claim = %q, want current owner %q", got, newToken)
	}
	if err := newStore.Save(ctx, "invalid", "outdated schema"); err != nil {
		t.Fatal(err)
	}
	if err := newStore.Delete(ctx, "invalid"); err != nil {
		t.Fatal(err)
	}
	if fields := rdb.HGetAll(ctx, checkpointKey(kind, job.ID)).Val(); len(fields) != 1 || fields["completed"] != "first successful result" {
		t.Fatalf("deleting an invalid step changed unrelated checkpoints: %v", fields)
	}
	if err := newStore.Delete(ctx, "invalid"); err != nil {
		t.Fatalf("delete already missing checkpoint: %v", err)
	}
	if err := newStore.Save(ctx, "invalid", "validated replacement"); err != nil {
		t.Fatal(err)
	}
	if result, found, err := newStore.Load(ctx, "invalid"); err != nil || !found || result != "validated replacement" {
		t.Fatalf("regenerated checkpoint = (%q, %v, %v)", result, found, err)
	}
	if err := rdb.Del(ctx, claimKey(kind, job.ID)).Err(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := newStore.Load(ctx, "completed"); !errors.Is(err, ErrClaimLost) {
		t.Fatalf("expired claim load = %v, want ErrClaimLost", err)
	}
	if err := newStore.Save(ctx, "late-result", "expired result"); !errors.Is(err, ErrClaimLost) {
		t.Fatalf("expired claim save = %v, want ErrClaimLost", err)
	}
	if err := newStore.Delete(ctx, "completed"); !errors.Is(err, ErrClaimLost) {
		t.Fatalf("expired claim delete = %v, want ErrClaimLost", err)
	}
}

func TestCheckpointCleanupAndFreshJobIsolation(t *testing.T) {
	oldMaxAttempts := MaxAttempts
	MaxAttempts = 1
	t.Cleanup(func() { MaxAttempts = oldMaxAttempts })
	for _, path := range []string{"inline", "worker"} {
		for _, outcome := range []string{"success", "abandoned", "deleted"} {
			t.Run(path+"/"+outcome, func(t *testing.T) {
				rdb := requireRedis(t)
				ctx := context.Background()
				kind := testKind(t)
				q := NewQueue(rdb)
				job := enqueueCheckpointTestJob(t, q, kind)
				calls := 0
				handler := func(ctx context.Context, _ Job) error {
					_, err := checkpoint.Do(ctx, "model", func() (string, error) {
						calls++
						return "successful model output", nil
					})
					if err != nil {
						return err
					}
					switch outcome {
					case "abandoned":
						return errFake
					case "deleted":
						return workguard.ErrDeleted
					default:
						return nil
					}
				}
				if path == "inline" {
					claimCheckpointTestJob(t, q, job)
					err := q.Execute(ctx, job, time.Minute, handler)
					if outcome == "abandoned" && !errors.Is(err, errFake) || outcome != "abandoned" && err != nil {
						t.Fatalf("execute %s = %v", outcome, err)
					}
				} else {
					runCheckpointTestWorker(t, NewWorker(rdb, kind, 1, time.Minute, handler))
				}
				assertCheckpointJobCleaned(t, rdb, job)

				fresh := enqueueCheckpointTestJob(t, q, kind)
				if fresh.ID == job.ID {
					t.Fatal("new enqueue reused the previous job ID")
				}
				claimCheckpointTestJob(t, q, fresh)
				if err := q.Execute(ctx, fresh, time.Minute, func(ctx context.Context, _ Job) error {
					value, err := checkpoint.Do(ctx, "model", func() (string, error) {
						calls++
						return "fresh output", nil
					})
					if value != "fresh output" {
						t.Fatalf("new job reused old checkpoint: %q", value)
					}
					return err
				}); err != nil {
					t.Fatal(err)
				}
				if calls != 2 {
					t.Fatalf("generation calls = %d, want one for each job", calls)
				}
				assertCheckpointJobCleaned(t, rdb, fresh)
			})
		}
	}
}

func TestCheckpointStorageFailureCannotBeSwallowed(t *testing.T) {
	for _, path := range []string{"inline", "inline-without-renewal", "worker"} {
		for _, operation := range []string{"load", "save"} {
			t.Run(path+"/"+operation, func(t *testing.T) {
				rdb := requireRedis(t)
				ctx := context.Background()
				kind := testKind(t)
				q := NewQueue(rdb)
				job := enqueueCheckpointTestJob(t, q, kind)
				breakStore := func() {
					if err := rdb.Set(ctx, checkpointKey(kind, job.ID), "wrong Redis type", 0).Err(); err != nil {
						t.Fatal(err)
					}
				}
				if operation == "load" {
					breakStore()
				}
				var checkpointErr error
				handler := func(ctx context.Context, _ Job) error {
					_, checkpointErr = checkpoint.Do(ctx, "model", func() (string, error) {
						breakStore()
						return "generated successfully", nil
					})
					if checkpointErr == nil {
						t.Fatal("checkpoint operation unexpectedly succeeded")
					}
					if err := workguard.Check(ctx); err == nil {
						t.Fatal("checkpoint storage error did not stop guarded work")
					}
					return nil // A handler accepting an optional-stage failure.
				}
				if path == "worker" {
					runCheckpointTestWorker(t, NewWorker(rdb, kind, 1, time.Minute, handler))
				} else {
					claimCheckpointTestJob(t, q, job)
					ttl := time.Minute
					if path == "inline-without-renewal" {
						ttl = 0
					}
					if err := q.Execute(ctx, job, ttl, handler); err == nil {
						t.Fatal("Execute consumed job despite checkpoint storage error")
					}
				}
				if checkpointErr == nil {
					t.Fatal("handler was not invoked")
				}
				if pending, err := q.Pending(ctx, kind, job.DedupeKey); err != nil || !pending {
					t.Fatalf("pending = (%v, %v), want failed job retained for retry", pending, err)
				}
				if n := rdb.LLen(ctx, processingKey(kind)).Val(); n != 1 {
					t.Fatalf("processing length = %d, want failed job retained", n)
				}
			})
		}
	}
}

func enqueueCheckpointTestJob(t *testing.T, q *Queue, kind Kind) Job {
	t.Helper()
	job, added, err := q.Enqueue(context.Background(), kind, "same-dedupe-key", nil)
	if err != nil || !added {
		t.Fatalf("enqueue job = %+v, %v, %v", job, added, err)
	}
	t.Cleanup(func() {
		if err := q.rdb.Del(context.Background(), queueKey(kind), processingKey(kind), dedupeSetKey(kind), claimKey(kind, job.ID), checkpointKey(kind, job.ID)).Err(); err != nil {
			t.Errorf("clean checkpoint test job: %v", err)
		}
	})
	return job
}

func claimCheckpointTestJob(t *testing.T, q *Queue, job Job) {
	t.Helper()
	if claimed, err := q.TryClaimByID(context.Background(), job, time.Minute); err != nil || !claimed {
		t.Fatalf("claim job = %v, %v", claimed, err)
	}
}

func runCheckpointTestWorker(t *testing.T, worker *Worker) {
	t.Helper()
	raw, err := worker.rdb.RPopLPush(context.Background(), queueKey(worker.kind), processingKey(worker.kind)).Result()
	if err != nil {
		t.Fatal(err)
	}
	worker.run(raw, nil)
}

func assertCheckpointJobCleaned(t *testing.T, rdb *redis.Client, job Job) {
	t.Helper()
	ctx := context.Background()
	for _, key := range []string{checkpointKey(job.Kind, job.ID), claimKey(job.Kind, job.ID), processingKey(job.Kind)} {
		if n, err := rdb.Exists(ctx, key).Result(); err != nil || n != 0 {
			t.Fatalf("terminal job key %q still exists: %d, %v", key, n, err)
		}
	}
	if pending, err := NewQueue(rdb).Pending(ctx, job.Kind, job.DedupeKey); err != nil || pending {
		t.Fatalf("terminal job pending = %v, %v", pending, err)
	}
}
