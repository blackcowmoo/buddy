package workslot

import (
	"context"
	"errors"
	"sync"
	"testing"
	"testing/synctest"
)

func TestParkedHandlersAdmitIndependentWork(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		pool := New(2)
		release := make(chan struct{})
		var done sync.WaitGroup
		for range 2 {
			done.Add(1)
			go func() {
				defer done.Done()
				if err := pool.Run(context.Background(), nil, func(ctx context.Context) error {
					return Wait(ctx, func() error { <-release; return nil })
				}); err != nil {
					t.Errorf("parked job: %v", err)
				}
			}()
		}
		synctest.Wait()
		independent := false
		if err := pool.Run(context.Background(), nil, func(context.Context) error {
			independent = true
			return nil
		}); err != nil {
			t.Fatalf("independent job: %v", err)
		}
		if !independent {
			t.Fatal("independent job did not run while both earlier jobs were parked")
		}
		close(release)
		done.Wait()
		assertActive(t, pool, 0)
	})
}

func TestPoolBoundsActiveHandlers(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		pool := New(1)
		release := make(chan struct{})
		firstDone := make(chan error, 1)
		go func() {
			firstDone <- pool.Run(context.Background(), nil, func(context.Context) error {
				<-release
				return nil
			})
		}()
		synctest.Wait()
		secondRan := false
		secondDone := make(chan error, 1)
		go func() {
			secondDone <- pool.Run(context.Background(), nil, func(context.Context) error {
				secondRan = true
				return nil
			})
		}()
		synctest.Wait()
		if secondRan {
			t.Fatal("second handler ran while the first held the only slot")
		}
		assertActive(t, pool, 1)
		close(release)
		assertResult(t, firstDone, nil)
		assertResult(t, secondDone, nil)
		if !secondRan {
			t.Fatal("second handler did not run after the first finished")
		}
		assertActive(t, pool, 0)
	})
}

func TestOverlappingWaitsShareSlotAndReacquireOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		pool := New(1)
		firstRelease, secondRelease := make(chan struct{}), make(chan struct{})
		firstReturned, secondReturned := make(chan struct{}), make(chan struct{})
		otherRelease := make(chan struct{})
		yields := 0
		done := make(chan error, 1)
		go func() {
			done <- pool.Run(context.Background(), func() { yields++ }, func(ctx context.Context) error {
				var children sync.WaitGroup
				for _, pair := range []struct{ release, returned chan struct{} }{
					{firstRelease, firstReturned}, {secondRelease, secondReturned},
				} {
					children.Add(1)
					go func() {
						defer children.Done()
						if err := Wait(ctx, func() error { <-pair.release; return nil }); err != nil {
							t.Errorf("parallel wait: %v", err)
						}
						close(pair.returned)
					}()
				}
				children.Wait()
				assertActive(t, pool, 1)
				// A later stage yields again but the dispatch notification is once
				// per job, even across separate rounds of waiting.
				return Wait(ctx, func() error { return nil })
			})
		}()
		synctest.Wait()
		assertActive(t, pool, 0)
		otherDone := make(chan error, 1)
		go func() {
			otherDone <- pool.Run(context.Background(), nil, func(context.Context) error {
				<-otherRelease
				return nil
			})
		}()
		synctest.Wait()
		close(firstRelease)
		synctest.Wait()
		assertClosed(t, firstReturned, true)
		assertClosed(t, secondReturned, false)
		close(secondRelease)
		synctest.Wait()
		assertClosed(t, secondReturned, false)
		assertActive(t, pool, 1)
		close(otherRelease)
		assertResult(t, otherDone, nil)
		assertResult(t, done, nil)
		assertClosed(t, secondReturned, true)
		if yields != 1 {
			t.Fatalf("yield notifications = %d, want 1", yields)
		}
		assertActive(t, pool, 0)
	})
}

func TestCancellationDuringReacquireDoesNotLeakCapacity(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		pool := New(1)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		release := make(chan struct{})
		otherRelease := make(chan struct{})
		done, otherDone := make(chan error, 1), make(chan error, 1)
		go func() {
			done <- pool.Run(ctx, nil, func(ctx context.Context) error {
				return Wait(ctx, func() error { <-release; return nil })
			})
		}()
		synctest.Wait()
		go func() {
			otherDone <- pool.Run(context.Background(), nil, func(context.Context) error {
				<-otherRelease
				return nil
			})
		}()
		synctest.Wait()
		close(release)
		synctest.Wait()
		select {
		case err := <-done:
			t.Fatalf("wait returned before capacity was available or canceled: %v", err)
		default:
		}
		cancel()
		assertResult(t, done, context.Canceled)
		assertActive(t, pool, 1)
		close(otherRelease)
		assertResult(t, otherDone, nil)
		assertActive(t, pool, 0)
		if err := pool.Run(context.Background(), nil, func(context.Context) error { return nil }); err != nil {
			t.Fatalf("subsequent job: %v", err)
		}
	})
}

func TestNewWaitDuringReacquireKeepsSingleOwnership(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		pool := New(1)
		firstRelease, secondRelease := make(chan struct{}), make(chan struct{})
		startSecond, secondAttempted := make(chan struct{}), make(chan struct{})
		otherRelease := make(chan struct{})
		done, otherDone := make(chan error, 1), make(chan error, 1)
		go func() {
			done <- pool.Run(context.Background(), nil, func(ctx context.Context) error {
				var children sync.WaitGroup
				children.Add(2)
				go func() {
					defer children.Done()
					if err := Wait(ctx, func() error { <-firstRelease; return nil }); err != nil {
						t.Errorf("first wait: %v", err)
					}
				}()
				go func() {
					defer children.Done()
					<-startSecond
					close(secondAttempted)
					if err := Wait(ctx, func() error { <-secondRelease; return nil }); err != nil {
						t.Errorf("second wait: %v", err)
					}
				}()
				children.Wait()
				assertActive(t, pool, 1)
				return nil
			})
		}()
		synctest.Wait()
		go func() {
			otherDone <- pool.Run(context.Background(), nil, func(context.Context) error {
				<-otherRelease
				return nil
			})
		}()
		synctest.Wait()
		close(firstRelease)
		synctest.Wait()
		close(startSecond)
		<-secondAttempted
		close(otherRelease)
		assertResult(t, otherDone, nil)
		synctest.Wait()
		assertActive(t, pool, 0)
		close(secondRelease)
		assertResult(t, done, nil)
		assertActive(t, pool, 0)
	})
}

func TestCanceledAdmissionDoesNotRunHandler(t *testing.T) {
	pool := New(0)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := pool.Run(ctx, nil, func(context.Context) error {
		t.Fatal("canceled handler ran")
		return nil
	}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run error = %v, want cancellation", err)
	}
	assertActive(t, pool, 0)
}

func TestPassThroughWithoutPoolOrSlot(t *testing.T) {
	want := errors.New("work error")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var pool *Pool
	if err := pool.Run(ctx, func() { t.Fatal("nil pool yielded") }, func(got context.Context) error {
		if got != ctx {
			t.Fatal("nil pool replaced the context")
		}
		return want
	}); !errors.Is(err, want) {
		t.Fatalf("nil pool error = %v, want %v", err, want)
	}
	for _, ctx := range []context.Context{nil, context.Background(), ctx} {
		if err := Wait(ctx, func() error { return want }); !errors.Is(err, want) {
			t.Fatalf("unbound Wait error = %v, want %v", err, want)
		}
	}
}

func TestErrorsAndPanicsReleaseCapacity(t *testing.T) {
	for _, phase := range []string{"handler", "wait", "yield"} {
		for _, panics := range []bool{false, true} {
			if phase == "yield" && !panics {
				continue
			}
			t.Run(phase+map[bool]string{false: "/error", true: "/panic"}[panics], func(t *testing.T) {
				pool := New(1)
				want := errors.New("failure")
				fail := func() error {
					if panics {
						panic(want)
					}
					return want
				}
				var gotPanic any
				var gotErr error
				func() {
					defer func() { gotPanic = recover() }()
					gotErr = pool.Run(context.Background(), func() {
						if phase == "yield" {
							panic(want)
						}
					}, func(ctx context.Context) error {
						if phase == "handler" {
							return fail()
						}
						return Wait(ctx, fail)
					})
				}()
				if panics && gotPanic != want {
					t.Fatalf("panic = %v, want %v", gotPanic, want)
				}
				if !panics && !errors.Is(gotErr, want) {
					t.Fatalf("error = %v, want %v", gotErr, want)
				}
				assertActive(t, pool, 0)
				if err := pool.Run(context.Background(), nil, func(context.Context) error { return nil }); err != nil {
					t.Fatalf("subsequent job: %v", err)
				}
			})
		}
	}
}

func assertActive(t *testing.T, pool *Pool, want int) {
	t.Helper()
	if got := len(pool.active); got != want {
		t.Fatalf("active slots = %d, want %d", got, want)
	}
}

func assertResult(t *testing.T, result <-chan error, want error) {
	t.Helper()
	if got := <-result; !errors.Is(got, want) {
		t.Fatalf("result = %v, want %v", got, want)
	}
}

func assertClosed(t *testing.T, ch <-chan struct{}, want bool) {
	t.Helper()
	select {
	case <-ch:
		if !want {
			t.Fatal("unexpected channel closure")
		}
	default:
		if want {
			t.Fatal("channel has not closed")
		}
	}
}
