package asyncjob

import (
	"context"
	"fmt"
	"testing"
	"time"

	"buddy/server/internal/llm"
	"buddy/server/internal/workguard"
	"buddy/server/internal/workslot"
)

func TestWorkerParkedModelCallsDoNotStarveQueuedJobs(t *testing.T) {
	for _, concurrency := range []int{1, 4} {
		t.Run(fmt.Sprintf("concurrency=%d", concurrency), func(t *testing.T) {
			rdb := requireRedis(t)
			kind := testKind(t)
			queue := NewQueue(rdb)
			models := llm.NewCallQueue()
			blockedCtx, cancelBlocked := context.WithCancel(context.Background())
			runCtx, stop := context.WithCancel(context.Background())
			parked := make(chan string, concurrency)
			started := make(chan string, concurrency+1)
			completed := make(chan string, concurrency+1)
			handler := func(ctx context.Context, job Job) error {
				model := "stalled-model"
				if job.DedupeKey == "healthy" {
					model = "healthy-model"
				}
				err := workslot.Wait(ctx, func() error {
					if model == "stalled-model" {
						parked <- job.DedupeKey
					}
					return models.Do(ctx, model, func() error {
						started <- job.DedupeKey
						if job.DedupeKey == "blocked-0" {
							<-blockedCtx.Done()
							return workguard.ErrDeleted
						}
						return nil
					})
				})
				completed <- job.DedupeKey
				return err
			}
			worker := NewWorker(rdb, kind, concurrency, time.Minute, handler)
			done := make(chan struct{})
			go func() {
				defer close(done)
				worker.Run(runCtx)
			}()
			defer func() {
				stop()
				cancelBlocked()
				select {
				case <-done:
				case <-time.After(10 * time.Second):
					t.Error("worker did not drain parked jobs on shutdown")
				}
			}()
			enqueue := func(key string) {
				t.Helper()
				if _, added, err := queue.Enqueue(context.Background(), kind, key, nil); err != nil || !added {
					t.Fatalf("enqueue %s: added=%v, err=%v", key, added, err)
				}
			}
			receive := func(ch <-chan string) string {
				t.Helper()
				select {
				case key := <-ch:
					return key
				case <-time.After(5 * time.Second):
					t.Fatal("worker failed to advance independently runnable work")
					return ""
				}
			}

			// Occupy every old whole-handler slot, including calls queued behind
			// the stalled request. Establish admission order with handshakes.
			for i := 0; i < concurrency; i++ {
				key := fmt.Sprintf("blocked-%d", i)
				enqueue(key)
				if got := receive(parked); got != key {
					t.Fatalf("parked %s, want %s", got, key)
				}
				if i == 0 && receive(started) != key {
					t.Fatal("first model request did not start")
				}
			}
			enqueue("healthy")
			if got := receive(started); got != "healthy" {
				t.Fatalf("started %s while the stalled model was occupied", got)
			}
			if got := receive(completed); got != "healthy" {
				t.Fatalf("completed %s before the stalled model was canceled", got)
			}
			// Yielding is scheduling, not durable completion or loss of ownership.
			for i := 0; i < concurrency; i++ {
				key := fmt.Sprintf("blocked-%d", i)
				if pending, err := queue.Pending(context.Background(), kind, key); err != nil || !pending {
					t.Fatalf("parked job %s lost pending state: %v, %v", key, pending, err)
				}
				if _, added, err := queue.Enqueue(context.Background(), kind, key, nil); err != nil || added {
					t.Fatalf("parked job %s lost deduplication: %v, %v", key, added, err)
				}
			}
			cancelBlocked()
			seen := map[string]bool{}
			for i := 0; i < concurrency; i++ {
				key := receive(completed)
				if seen[key] {
					t.Fatalf("job %s executed twice", key)
				}
				seen[key] = true
			}
		})
	}
}
