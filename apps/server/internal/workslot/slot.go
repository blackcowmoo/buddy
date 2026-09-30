// Package workslot bounds active job handlers while allowing handlers parked
// on independent external work to make room for another job.
package workslot

import (
	"context"
	"sync"
)

type Pool struct {
	active chan struct{}
}

// New creates a pool with at least one active handler slot. Parked handlers
// retain their own execution state and do not count against this limit.
func New(size int) *Pool {
	return &Pool{active: make(chan struct{}, max(1, size))}
}

type slotKey struct{}

type slot struct {
	pool    *Pool
	mu      sync.Mutex
	waiting int
	held    bool
	yielded bool
	onYield func()
}

// Run reserves one slot for work. Wait can temporarily release it through
// the supplied context, including from parallel child calls. Work must join
// those calls before returning. onYield runs only on the first release, so a
// dispatcher can admit another job without waiting for this one to finish.
// A nil pool calls work directly.
func (p *Pool) Run(ctx context.Context, onYield func(), work func(context.Context) error) error {
	if p == nil {
		return work(ctx)
	}
	if err := p.acquire(ctx); err != nil {
		return err
	}
	s := &slot{pool: p, held: true, onYield: onYield}
	defer func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.held {
			p.release()
			s.held = false
		}
	}()
	return work(context.WithValue(ctx, slotKey{}, s))
}

func (p *Pool) acquire(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case p.active <- struct{}{}:
		// Cancellation and a newly available slot can become ready together.
		// Return the slot instead of starting canceled work in that race.
		if err := ctx.Err(); err != nil {
			p.release()
			return err
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (p *Pool) release() {
	<-p.active
}

// Wait releases the handler slot while work waits for a model queue or an
// external response. Overlapping waits share the release: the final return
// restores the slot before its caller continues. Contexts without a Run slot
// call work directly. Cancellation while restoring capacity is returned even
// if the external work succeeded.
func Wait(ctx context.Context, work func() error) (err error) {
	if ctx == nil {
		return work()
	}
	s, ok := ctx.Value(slotKey{}).(*slot)
	if !ok {
		return work()
	}
	onYield := s.park()
	defer func() {
		if resumeErr := s.resume(ctx); resumeErr != nil {
			err = resumeErr
		}
	}()
	if onYield != nil {
		onYield()
	}
	return work()
}

func (s *slot) park() func() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.waiting++
	if s.held {
		s.pool.release()
		s.held = false
	}
	if s.yielded {
		return nil
	}
	s.yielded = true
	return s.onYield
}

func (s *slot) resume(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.waiting--
	if s.waiting != 0 {
		return nil
	}
	// Keep this handler's mutex while acquiring, so a parallel new Wait cannot
	// race the final return and acquire a second slot. Other handlers use their
	// own mutexes and can continue releasing capacity while this one is parked.
	if err := s.pool.acquire(ctx); err != nil {
		return err
	}
	s.held = true
	return nil
}
