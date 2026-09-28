package llm

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
)

func TestCallQueueIsIndependentPerKey(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		q := NewCallQueue()
		release := make(chan struct{})
		unblock := sync.OnceFunc(func() { close(release) })
		defer unblock()
		go func() {
			if err := q.Do(context.Background(), "llm2", func() error {
				<-release
				return nil
			}); err != nil {
				t.Errorf("first call: %v", err)
			}
		}()
		synctest.Wait()

		sameKeyStarted, otherKeyStarted := false, false
		for _, call := range []struct {
			key     string
			started *bool
		}{{"llm2", &sameKeyStarted}, {"llm1", &otherKeyStarted}} {
			go func() {
				if err := q.Do(context.Background(), call.key, func() error {
					*call.started = true
					return nil
				}); err != nil {
					t.Errorf("call for %s: %v", call.key, err)
				}
			}()
		}
		synctest.Wait()
		if sameKeyStarted || !otherKeyStarted {
			t.Fatalf("while llm2 is occupied: same key started = %v, other key started = %v", sameKeyStarted, otherKeyStarted)
		}
		unblock()
		synctest.Wait()
		if !sameKeyStarted {
			t.Fatal("same-key call did not start after the first call released its lane")
		}
		assertCallQueueEmpty(t, q)
	})
}

func TestCallQueuePreservesFIFOThroughCancellation(t *testing.T) {
	for _, tc := range []struct {
		name     string
		canceled int
	}{
		{"none", -1},
		{"first waiter", 1},
		{"middle waiter", 2},
		{"last waiter", 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var q CallQueue // The zero value must work as well as NewCallQueue.
				release := make(chan struct{})
				unblock := sync.OnceFunc(func() { close(release) })
				defer unblock()
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				var calls []int
				var results [4]error
				for i := range results {
					callCtx := context.Background()
					if i == tc.canceled {
						callCtx = ctx
					}
					go func() {
						results[i] = q.Do(callCtx, "llm", func() error {
							calls = append(calls, i)
							if i == 0 {
								<-release
							}
							return nil
						})
					}()
					// Establish registration order without relying on scheduling or sleeps.
					synctest.Wait()
				}
				cancel()
				synctest.Wait()
				if !slices.Equal(calls, []int{0}) {
					t.Fatalf("calls before release = %v, want [0]", calls)
				}
				unblock()
				synctest.Wait()

				var want []int
				for i, err := range results {
					var wantErr error
					if i == tc.canceled {
						wantErr = context.Canceled
					} else {
						want = append(want, i)
					}
					if !errors.Is(err, wantErr) {
						t.Errorf("call %d error = %v, want %v", i, err, wantErr)
					}
				}
				if !slices.Equal(calls, want) {
					t.Errorf("call order = %v, want %v", calls, want)
				}
				assertCallQueueEmpty(t, &q)
			})
		})
	}
}

func TestCallQueueCancellationAfterHandoffKeepsOwnership(t *testing.T) {
	q := NewCallQueue()
	lane, err := q.acquire(context.Background(), "llm")
	if err != nil {
		t.Fatal(err)
	}
	first := &callWaiter{ready: make(chan struct{})}
	next := &callWaiter{ready: make(chan struct{})}
	lane.waiters = append(lane.waiters, first, next)
	q.release("llm", lane)

	// Drive the cancellation/handoff ordering explicitly: acquire must keep
	// the already-handed-off slot so Do's deferred release can advance it.
	if !q.cancel(lane, first) {
		t.Fatal("cancellation discarded the handed-off slot")
	}
	select {
	case <-next.ready:
		t.Fatal("next waiter started before the handed-off slot was released")
	default:
	}
	q.release("llm", lane)
	select {
	case <-next.ready:
	default:
		t.Fatal("next waiter did not receive the slot")
	}
	q.release("llm", lane)
	assertCallQueueEmpty(t, q)
}

func TestCallQueueReleasesFailedCalls(t *testing.T) {
	for _, panics := range []bool{false, true} {
		name := "error"
		if panics {
			name = "panic"
		}
		t.Run(name, func(t *testing.T) {
			q := NewCallQueue()
			failure := errors.New("call failed")
			func() {
				defer func() {
					got := recover()
					if panics && got != failure {
						t.Errorf("panic = %v, want %v", got, failure)
					} else if !panics && got != nil {
						t.Errorf("unexpected panic: %v", got)
					}
				}()
				err := q.Do(context.Background(), "llm", func() error {
					if panics {
						panic(failure)
					}
					return failure
				})
				if !errors.Is(err, failure) {
					t.Errorf("error = %v, want %v", err, failure)
				}
			}()
			assertCallQueueEmpty(t, q)
			if err := q.Do(context.Background(), "llm", func() error { return nil }); err != nil {
				t.Fatalf("reuse after failure: %v", err)
			}
			assertCallQueueEmpty(t, q)
		})
	}
}

func assertCallQueueEmpty(t *testing.T, q *CallQueue) {
	t.Helper()
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.lanes) != 0 {
		t.Fatalf("queue retained %d idle lane(s)", len(q.lanes))
	}
}
