package checkpoint_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"buddy/server/internal/checkpoint"
	"buddy/server/internal/checkpoint/checkpointtest"
	"buddy/server/internal/workguard"
)

func TestResumeOnlyCompletedSteps(t *testing.T) {
	var store checkpointtest.Memory
	first := checkpoint.Bind(context.Background(), &store)
	calls := 0
	generate := func() (string, error) { calls++; return "completed", nil }
	if _, err := checkpoint.Do(first, "draft:v1", generate); err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(first)
	_, err := checkpoint.Do(canceled, "judge:v1", func() (string, error) {
		cancel()
		return "late response", nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("interrupted step: %v", err)
	}
	resumed := checkpoint.Bind(context.Background(), &store)
	if got, err := checkpoint.Do(resumed, "draft:v1", generate); err != nil || got != "completed" || calls != 1 {
		t.Fatalf("replay = %q, %v, calls = %d", got, err, calls)
	}
	if _, err := checkpoint.Do(resumed, "judge:v1", generate); err != nil || calls != 2 {
		t.Fatalf("unfinished step retry = %v, calls = %d", err, calls)
	}
}

func TestFailedAndEmptyStepsRemainRetryable(t *testing.T) {
	for _, initial := range []struct {
		name, text string
		err        error
	}{
		{"failed", "partial", errors.New("model failed")},
		{"empty", "  \n", nil},
	} {
		t.Run(initial.name, func(t *testing.T) {
			var store checkpointtest.Memory
			ctx := checkpoint.Bind(context.Background(), &store)
			_, _ = checkpoint.Do(ctx, "step", func() (string, error) { return initial.text, initial.err })
			got, err := checkpoint.Do(ctx, "step", func() (string, error) { return "retry", nil })
			if got != "retry" || err != nil {
				t.Fatalf("retry = %q, %v", got, err)
			}
		})
	}
}

type failingStore struct {
	checkpointtest.Memory
	loadErr, saveErr, deleteErr error
}

func (s *failingStore) Load(ctx context.Context, key string) (string, bool, error) {
	if s.loadErr != nil {
		return "", false, s.loadErr
	}
	return s.Memory.Load(ctx, key)
}

func (s *failingStore) Save(ctx context.Context, key, value string) error {
	if s.saveErr != nil {
		return s.saveErr
	}
	return s.Memory.Save(ctx, key, value)
}

func (s *failingStore) Delete(ctx context.Context, key string) error {
	if s.deleteErr != nil {
		return s.deleteErr
	}
	return s.Memory.Delete(ctx, key)
}

func TestRejectedResultsAreRegeneratedWithoutLosingOtherSteps(t *testing.T) {
	var store checkpointtest.Memory
	ctx := checkpoint.Bind(context.Background(), &store)
	if !checkpoint.Bound(ctx) || checkpoint.Bound(context.Background()) {
		t.Fatal("checkpoint scope not detected")
	}
	if err := store.Save(ctx, "draft", "completed draft"); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(ctx, "final", "legacy invalid"); err != nil {
		t.Fatal(err)
	}
	valid := func(value string) bool { return value == "valid" }
	got, err := checkpoint.DoIf(ctx, "final", func() (string, error) { return "still invalid", nil }, valid)
	if got != "still invalid" || err != nil {
		t.Fatalf("invalid result = %q, %v", got, err)
	}
	if _, found, _ := store.Load(ctx, "final"); found {
		t.Fatal("invalid result stored")
	}
	got, err = checkpoint.DoIf(ctx, "final", func() (string, error) { return "valid", nil }, valid)
	if got != "valid" || err != nil {
		t.Fatalf("regenerated result = %q, %v", got, err)
	}
	if value, found, _ := store.Load(ctx, "draft"); !found || value != "completed draft" {
		t.Fatal("unrelated completed draft lost")
	}
	got, err = checkpoint.DoIf(ctx, "final", func() (string, error) { t.Fatal("validated result not reused"); return "", nil }, valid)
	if got != "valid" || err != nil {
		t.Fatalf("replayed result = %q, %v", got, err)
	}
}

func TestFailedInvalidationStopsAttempt(t *testing.T) {
	discardErr := errors.New("cannot discard checkpoint")
	store := &failingStore{deleteErr: discardErr}
	ctx := checkpoint.Bind(context.Background(), store)
	if err := store.Memory.Save(ctx, "step", "invalid"); err != nil {
		t.Fatal(err)
	}
	_, err := checkpoint.DoIf(ctx, "step", func() (string, error) { t.Fatal("generation after failed invalidation"); return "", nil }, func(string) bool { return false })
	if !errors.Is(err, discardErr) || !errors.Is(workguard.Check(ctx), discardErr) {
		t.Fatalf("invalidation error = %v", err)
	}
}

type cancelOnSaveStore struct {
	checkpointtest.Memory
	cancel context.CancelFunc
}

func (s *cancelOnSaveStore) Save(ctx context.Context, key, value string) error {
	err := s.Memory.Save(ctx, key, value)
	s.cancel()
	return err
}

func TestCancellationDuringSaveRetainsResultWithoutPublishingIt(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := &cancelOnSaveStore{cancel: cancel}
	_, err := checkpoint.Do(checkpoint.Bind(ctx, store), "step", func() (string, error) { return "saved", nil })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("save cancellation = %v", err)
	}
	got, err := checkpoint.Do(checkpoint.Bind(context.Background(), store), "step", func() (string, error) { t.Fatal("saved result lost on cancellation"); return "", nil })
	if got != "saved" || err != nil {
		t.Fatalf("saved result replay = %q, %v", got, err)
	}
}

func TestStorageFailureBlocksFallbackAndDetachedWrites(t *testing.T) {
	storageErr := errors.New("storage unavailable")
	for _, operation := range []string{"load", "save"} {
		t.Run(operation, func(t *testing.T) {
			store := &failingStore{}
			if operation == "load" {
				store.loadErr = storageErr
			} else {
				store.saveErr = storageErr
			}
			ctx := checkpoint.Bind(context.Background(), store)
			_, err := checkpoint.Do(ctx, "step", func() (string, error) { return "result", nil })
			if !errors.Is(err, storageErr) {
				t.Fatalf("Do = %v", err)
			}
			store.loadErr, store.saveErr = nil, nil
			_, err = checkpoint.Do(ctx, "fallback", func() (string, error) {
				t.Fatal("fallback executed after checkpoint failure")
				return "", nil
			})
			if !errors.Is(err, storageErr) {
				t.Fatalf("fallback = %v", err)
			}
			err = workguard.Commit(context.WithoutCancel(ctx), func(context.Context) error {
				t.Fatal("derived write executed after checkpoint failure")
				return nil
			})
			if !errors.Is(err, storageErr) {
				t.Fatalf("commit = %v", err)
			}
		})
	}
}

func TestConcurrentStepsShareCompletedResult(t *testing.T) {
	var store checkpointtest.Memory
	ctx := checkpoint.Bind(context.Background(), &store)
	var calls atomic.Int32
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			got, err := checkpoint.Do(ctx, "same-step", func() (string, error) {
				calls.Add(1)
				return "result", nil
			})
			if got != "result" || err != nil {
				t.Errorf("Do = %q, %v", got, err)
			}
		})
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("model calls = %d", calls.Load())
	}
}

func TestJSONSnapshotsAndDecodeFailure(t *testing.T) {
	var store checkpointtest.Memory
	type input struct {
		Profile  string
		Previous []string
	}
	ctx := checkpoint.Bind(context.Background(), &store)
	want := input{Profile: "original", Previous: []string{"previous"}}
	got, err := checkpoint.JSON(ctx, "input:v1", func() (input, error) { return want, nil })
	if err != nil || got.Profile != want.Profile {
		t.Fatalf("snapshot = %+v, %v", got, err)
	}
	got, err = checkpoint.JSON(checkpoint.Bind(context.Background(), &store), "input:v1", func() (input, error) {
		t.Fatal("snapshot regenerated on retry")
		return input{}, nil
	})
	if err != nil || got.Profile != want.Profile || len(got.Previous) != 1 || got.Previous[0] != "previous" {
		t.Fatalf("snapshot replay = %+v, %v", got, err)
	}
	if err := store.Save(ctx, "corrupt", "invalid JSON"); err != nil {
		t.Fatal(err)
	}
	_, err = checkpoint.JSON(ctx, "corrupt", func() (input, error) { t.Fatal("regenerated corrupt snapshot"); return input{}, nil })
	if err == nil || workguard.Check(ctx) == nil {
		t.Fatal("corrupt checkpoint did not stop attempt")
	}
}

func TestWithoutJobDoesNotReuseResults(t *testing.T) {
	calls := 0
	for range 2 {
		_, err := checkpoint.JSON(context.Background(), "key", func() (int, error) { calls++; return calls, nil })
		if err != nil {
			t.Fatal(err)
		}
		_, err = checkpoint.Do(context.Background(), "key", func() (string, error) { calls++; return "result", nil })
		if err != nil {
			t.Fatal(err)
		}
	}
	if calls != 4 {
		t.Fatalf("calls = %d", calls)
	}
}
