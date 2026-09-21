package workguard

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestDeletionCancelsRunningRequest(t *testing.T) {
	var deleted atomic.Bool
	ctx := Bind(context.Background(), func(context.Context) error {
		if deleted.Load() {
			return ErrDeleted
		}
		return nil
	})
	ticks := make(chan time.Time)
	started := make(chan struct{})
	result := make(chan error, 1)
	go func() {
		result <- run(ctx, ticks, func(ctx context.Context) error {
			close(started)
			<-ctx.Done()
			return ctx.Err()
		})
	}()
	<-started
	deleted.Store(true)
	ticks <- time.Time{}
	if err := <-result; !errors.Is(err, ErrDeleted) {
		t.Fatalf("error = %v", err)
	}
}

func TestLateResponseIsDiscarded(t *testing.T) {
	deleted := false
	ctx := Bind(context.Background(), func(context.Context) error {
		if deleted {
			return ErrDeleted
		}
		return nil
	})
	// A nil clock makes this independent of polling and wall-clock timing.
	err := run(ctx, nil, func(context.Context) error { deleted = true; return nil })
	if !errors.Is(err, ErrDeleted) {
		t.Fatalf("error = %v", err)
	}
}

func TestDetachedWorkSurvivesDisconnectButChecksDeletion(t *testing.T) {
	deleted := false
	parent, cancel := context.WithCancel(context.Background())
	ctx := Bind(parent, func(context.Context) error {
		if deleted {
			return ErrDeleted
		}
		return nil
	})
	cancel()
	detached := context.WithoutCancel(ctx)
	called := false
	if err := Run(detached, func(context.Context) error { called = true; return nil }); err != nil || !called {
		t.Fatalf("disconnect canceled durable work: %v", err)
	}
	deleted = true
	if err := Run(detached, func(context.Context) error { t.Fatal("called after deletion"); return nil }); !errors.Is(err, ErrDeleted) {
		t.Fatalf("error = %v", err)
	}
}

func TestLookupFailureDoesNotAuthorizeWork(t *testing.T) {
	failure := errors.New("primary unavailable")
	ctx := Bind(context.Background(), func(context.Context) error { return failure })
	if err := Run(ctx, func(context.Context) error { t.Fatal("called after failed check"); return nil }); !errors.Is(err, failure) {
		t.Fatalf("error = %v", err)
	}
}
