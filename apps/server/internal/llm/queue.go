package llm

import (
	"context"
	"errors"
	"sync"
)

// CallQueue serializes calls independently for each key. A key normally
// identifies one model behind one endpoint. Calls for different keys never
// wait on one another, while calls for the same key run in FIFO order.
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
	running bool
	waiters []*callWaiter
}

type callWaiter struct {
	ready    chan struct{}
	started  bool
	canceled bool
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
	}
	if !lane.running && len(lane.waiters) == 0 {
		lane.running = true
		q.mu.Unlock()
		return lane, nil
	}

	w := &callWaiter{ready: make(chan struct{})}
	lane.waiters = append(lane.waiters, w)
	q.mu.Unlock()

	select {
	case <-w.ready:
		return lane, nil
	case <-ctx.Done():
		// If release already handed this waiter the slot, keep ownership and
		// let Do's deferred release balance it. Otherwise remove it so a
		// canceled request does not leave a dead waiter in the lane.
		if q.cancel(key, lane, w) {
			return lane, nil
		}
		return nil, ctx.Err()
	}
}

// cancel returns true when release concurrently handed w the slot. In that
// case the caller must proceed to release it, even though its context ended.
func (q *CallQueue) cancel(key string, lane *callLane, w *callWaiter) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if w.started {
		return true
	}
	w.canceled = true
	for i, queued := range lane.waiters {
		if queued != w {
			continue
		}
		lane.waiters = append(lane.waiters[:i], lane.waiters[i+1:]...)
		break
	}
	q.removeIdleLaneLocked(key, lane)
	return false
}

func (q *CallQueue) release(key string, lane *callLane) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for len(lane.waiters) > 0 {
		w := lane.waiters[0]
		lane.waiters = lane.waiters[1:]
		if w.canceled {
			continue
		}
		w.started = true
		close(w.ready)
		return
	}
	lane.running = false
	q.removeIdleLaneLocked(key, lane)
}

func (q *CallQueue) removeIdleLaneLocked(key string, lane *callLane) {
	if !lane.running && len(lane.waiters) == 0 && q.lanes[key] == lane {
		delete(q.lanes, key)
	}
}
