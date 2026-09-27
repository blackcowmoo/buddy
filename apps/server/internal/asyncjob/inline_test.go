package asyncjob

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"testing/synctest"
)

func TestInlineRunnerDeduplicatesAndReleasesFinishedWork(t *testing.T) {
	for _, workErr := range []error{nil, errors.New("generation failed")} {
		name := "success"
		if workErr != nil {
			name = "failure"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var runner InlineRunner
				var calls atomic.Int32
				release := make(chan struct{})
				defer close(release)
				work := func(ctx context.Context) error {
					if ctx.Done() != nil {
						t.Error("background work has a cancellation channel")
					}
					calls.Add(1)
					<-release
					return workErr
				}
				for range 20 {
					go runner.Start("word", "test inline", work)
				}
				synctest.Wait()
				if got := calls.Load(); got != 1 {
					t.Fatalf("overlapping calls = %d, want 1", got)
				}

				// A different key and an independently owned runner must both
				// proceed while the first key is still occupied.
				runner.Start("other-word", "test inline", work)
				var other InlineRunner
				other.Start("word", "test inline", work)
				synctest.Wait()
				if got := calls.Load(); got != 3 {
					t.Fatalf("independent calls = %d, want 3", got)
				}
				for range 3 {
					release <- struct{}{}
				}
				synctest.Wait()

				runner.Start("word", "test inline", work)
				synctest.Wait()
				if got := calls.Load(); got != 4 {
					t.Fatalf("calls after completion = %d, want 4", got)
				}
			})
		})
	}
}
