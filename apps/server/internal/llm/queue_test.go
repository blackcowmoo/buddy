package llm

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestCallQueueIsIndependentPerKey(t *testing.T) {
	q := NewCallQueue()
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	sameKeyStarted := make(chan struct{})
	otherKeyStarted := make(chan struct{})
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := q.Do(context.Background(), "llm2", func() error {
			close(firstStarted)
			<-releaseFirst
			return nil
		}); err != nil {
			t.Errorf("first call: %v", err)
		}
	}()
	<-firstStarted

	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := q.Do(context.Background(), "llm2", func() error {
			close(sameKeyStarted)
			return nil
		}); err != nil {
			t.Errorf("same-key call: %v", err)
		}
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := q.Do(context.Background(), "llm1", func() error {
			close(otherKeyStarted)
			return nil
		}); err != nil {
			t.Errorf("other-key call: %v", err)
		}
	}()

	waitForCallQueueSignal(t, otherKeyStarted, "other-key call")
	select {
	case <-sameKeyStarted:
		t.Fatal("same-key call started before the first call released its lane")
	default:
	}

	close(releaseFirst)
	<-sameKeyStarted
	wg.Wait()
}

func TestCallQueueCanceledWaiterDoesNotBlockNextCall(t *testing.T) {
	q := NewCallQueue()
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- q.Do(context.Background(), "llm", func() error {
			close(firstStarted)
			<-releaseFirst
			return nil
		})
	}()
	<-firstStarted

	ctx, cancel := context.WithCancel(context.Background())
	secondDone := make(chan error, 1)
	go func() {
		secondDone <- q.Do(ctx, "llm", func() error {
			return nil
		})
	}()
	waitForWaiters(t, q, "llm", 1)
	cancel()
	if err := <-secondDone; err != context.Canceled {
		t.Fatalf("canceled waiter error = %v, want context.Canceled", err)
	}

	thirdStarted := make(chan struct{})
	thirdDone := make(chan error, 1)
	go func() {
		thirdDone <- q.Do(context.Background(), "llm", func() error {
			close(thirdStarted)
			return nil
		})
	}()
	select {
	case <-thirdStarted:
		t.Fatal("next call started before the first call released its lane")
	default:
	}
	close(releaseFirst)
	if err := <-firstDone; err != nil {
		t.Fatalf("first call: %v", err)
	}
	if err := <-thirdDone; err != nil {
		t.Fatalf("next call: %v", err)
	}
	<-thirdStarted
}

func waitForWaiters(t *testing.T, q *CallQueue, key string, want int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		q.mu.Lock()
		lane := q.lanes[key]
		got := 0
		if lane != nil {
			got = len(lane.waiters)
		}
		q.mu.Unlock()
		if got == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("waiter count for %q did not reach %d", key, want)
}

func waitForCallQueueSignal(t *testing.T, signal <-chan struct{}, name string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for %s", name)
	}
}
