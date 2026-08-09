package concurrent

import (
	"sync"
	"sync/atomic"
	"testing"
)

// TestRunExecutesAllAndWaits verifies both that every fn actually runs and
// that Run doesn't return until all of them have — a mutex-guarded slice
// records completions, and its length is only trustworthy to assert on once
// Run has returned.
func TestRunExecutesAllAndWaits(t *testing.T) {
	const n = 20

	var mu sync.Mutex
	var completed []int

	fns := make([]func(), n)
	for i := 0; i < n; i++ {
		i := i
		fns[i] = func() {
			mu.Lock()
			completed = append(completed, i)
			mu.Unlock()
		}
	}

	Run(fns...)

	mu.Lock()
	defer mu.Unlock()
	if len(completed) != n {
		t.Fatalf("completed = %d entries, want %d — Run returned before every fn finished", len(completed), n)
	}

	seen := make(map[int]bool, n)
	for _, v := range completed {
		seen[v] = true
	}
	for i := 0; i < n; i++ {
		if !seen[i] {
			t.Errorf("fn %d never ran", i)
		}
	}
}

// TestRunAtomicCounter is a second, simpler witness of the same "all fns ran
// before Run returns" property using an atomic counter instead of a
// mutex-guarded slice.
func TestRunAtomicCounter(t *testing.T) {
	const n = 50

	var count int64
	fns := make([]func(), n)
	for i := 0; i < n; i++ {
		fns[i] = func() { atomic.AddInt64(&count, 1) }
	}

	Run(fns...)

	if got := atomic.LoadInt64(&count); got != n {
		t.Fatalf("count = %d, want %d", got, n)
	}
}

// TestRunZero verifies Run(...) with no functions is a safe, immediate no-op.
func TestRunZero(t *testing.T) {
	Run()
}
