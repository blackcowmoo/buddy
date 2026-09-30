package llm

import (
	"context"
	"errors"
	"slices"
	"sync"
)

type backgroundPriorityKey struct{}

// WithBackgroundPriority makes migration/backfill calls yield to ordinary
// calls waiting for the same model. Set it in the execution path so detached
// work and durable retries retain the policy. Running calls are not preempted.
func WithBackgroundPriority(ctx context.Context) context.Context {
	return context.WithValue(ctx, backgroundPriorityKey{}, true)
}

// CallQueue serializes calls independently for each key. A key normally
// identifies one model behind one endpoint. Calls for different keys never
// wait on one another. For the same key, ordinary calls precede background
// calls; calls within each priority run in FIFO order.
//
// The queue lives above the Client interface so it also protects clients that
// are not OpenAI implementations (for example, test doubles or another
// OpenAI-compatible transport). A caller should keep one CallQueue alongside
// the clients it shares across requests.
type CallQueue struct {
	mu    sync.Mutex
	lanes map[string]*callLane
}

type callLane struct {
	waiters []*callWaiter
}

type callWaiter struct {
	ready      chan struct{}
	started    bool
	background bool
}

// NewCallQueue returns an empty per-key call queue.
func NewCallQueue() *CallQueue {
	return &CallQueue{lanes: make(map[string]*callLane)}
}

// Do waits for the key's turn, runs fn, and releases the key even when fn
// returns an error or panics. A canceled context removes a still-waiting call
// from the queue. If cancellation races with the handoff, the call owns the
// slot and fn runs with the canceled context so the slot cannot be stranded.
func (q *CallQueue) Do(ctx context.Context, key string, fn func() error) error {
	if fn == nil {
		return errors.New("llm: nil queued call")
	}
	if q == nil {
		return fn()
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	lane, err := q.acquire(ctx, key)
	if err != nil {
		return err
	}
	defer q.release(key, lane)
	return fn()
}

func (q *CallQueue) acquire(ctx context.Context, key string) (*callLane, error) {
	q.mu.Lock()
	if q.lanes == nil {
		q.lanes = make(map[string]*callLane)
	}
	lane := q.lanes[key]
	if lane == nil {
		lane = &callLane{}
		q.lanes[key] = lane
		q.mu.Unlock()
		return lane, nil
	}

	// A lane exists only while a call owns it, including during a handoff.
	background, _ := ctx.Value(backgroundPriorityKey{}).(bool)
	w := &callWaiter{ready: make(chan struct{}), background: background}
	lane.waiters = append(lane.waiters, w)
	q.mu.Unlock()

	select {
	case <-w.ready:
		return lane, nil
	case <-ctx.Done():
		// If release already handed this waiter the slot, keep ownership and
		// let Do's deferred release balance it. Otherwise remove it so a
		// canceled request does not leave a dead waiter in the lane.
		if q.cancel(lane, w) {
			return lane, nil
		}
		return nil, ctx.Err()
	}
}

// cancel returns true when release concurrently handed w the slot. In that
// case the caller must proceed to release it, even though its context ended.
func (q *CallQueue) cancel(lane *callLane, w *callWaiter) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if w.started {
		return true
	}
	for i, queued := range lane.waiters {
		if queued != w {
			continue
		}
		lane.waiters = slices.Delete(lane.waiters, i, i+1)
		break
	}
	return false
}

func (q *CallQueue) release(key string, lane *callLane) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(lane.waiters) == 0 {
		delete(q.lanes, key)
		return
	}
	// The oldest background call runs only when no ordinary call is waiting.
	// Check at every handoff so a multi-call migration yields between stages.
	next := 0
	for i, w := range lane.waiters {
		if !w.background {
			next = i
			break
		}
	}
	w := lane.waiters[next]
	lane.waiters = slices.Delete(lane.waiters, next, next+1)
	w.started = true
	close(w.ready)
}
