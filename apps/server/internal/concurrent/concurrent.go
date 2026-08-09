// Package concurrent holds small generic concurrency helpers shared across
// packages that each independently reimplemented the same
// sync.WaitGroup-based "run these in parallel, wait for all" pattern.
package concurrent

import "sync"

// Run executes each fn concurrently and blocks until all have returned.
func Run(fns ...func()) {
	var wg sync.WaitGroup
	wg.Add(len(fns))
	for _, fn := range fns {
		go func(fn func()) {
			defer wg.Done()
			fn()
		}(fn)
	}
	wg.Wait()
}
