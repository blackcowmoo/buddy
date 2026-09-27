package asyncjob

import (
	"context"
	"log"
	"sync"
)

// InlineRunner suppresses overlapping work for the same key within one
// handler or process when Redis is absent. Its zero value is ready to use.
type InlineRunner struct {
	running sync.Map
}

// Start detaches work from the triggering request. Releasing the key on both
// success and failure lets a later poll recover work that is still pending.
func (r *InlineRunner) Start(key, errLabel string, work func(context.Context) error) {
	if _, loaded := r.running.LoadOrStore(key, struct{}{}); loaded {
		return
	}
	go func() {
		defer r.running.Delete(key)
		if err := work(context.Background()); err != nil {
			log.Printf("%s: %v", errLabel, err)
		}
	}()
}
