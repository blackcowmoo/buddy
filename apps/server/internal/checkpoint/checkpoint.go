// Package checkpoint retains completed steps for the lifetime of a durable job.
// The queue supplies the shared store; callers outside a job run normally.
package checkpoint

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"buddy/server/internal/workguard"
)

// Store is scoped to one job, independent of its worker or attempt. Its writes
// must reject attempts that no longer own the job and preserve the first result.
type Store interface {
	Load(context.Context, string) (string, bool, error)
	Save(context.Context, string, string) error
	Delete(context.Context, string) error
}

type contextKey struct{}

type state struct {
	store  Store
	mu     sync.Mutex
	err    error
	active map[string]chan struct{}
}

// Bound reports whether this request already has durable, job-scoped storage.
// Optional cross-request caches must not override its validation or lifecycle.
func Bound(ctx context.Context) bool {
	_, ok := ctx.Value(contextKey{}).(*state)
	return ok
}

// Bind makes storage failures fatal to the attempt, including callers that
// normally tolerate a model failure and fall back to an earlier draft. Keeping
// that failure in a workguard also protects detached derived writes.
func Bind(ctx context.Context, store Store) context.Context {
	s := &state{store: store, active: make(map[string]chan struct{})}
	ctx = context.WithValue(ctx, contextKey{}, s)
	return workguard.Bind(ctx, func(context.Context) error {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.err
	})
}

func (s *state) fail(operation string, err error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err == nil {
		s.err = fmt.Errorf("checkpoint: %s: %w", operation, err)
	}
	return s.err
}

// Do replays a successful nonempty result or persists a new one before handing
// it to the next step. Keys must include a version and all inputs that affect
// the result. Failed or interrupted calls are never recorded as completed.
func Do(ctx context.Context, key string, generate func() (string, error)) (string, error) {
	return DoIf(ctx, key, generate, nil)
}

// DoIf additionally requires reusable to accept a result before storing it.
// Rejected output is still returned for the caller's existing error/repair
// handling, but is regenerated on retry. It also discards stored output that
// no longer satisfies the current validator after a deployment.
func DoIf(ctx context.Context, key string, generate func() (string, error), reusable func(string) bool) (string, error) {
	if err := workguard.Check(ctx); err != nil {
		return "", err
	}
	s, ok := ctx.Value(contextKey{}).(*state)
	if !ok {
		return generate()
	}
	// Identical calls inside one attempt share a persisted result even when
	// parallel candidates reach the same step before its first call finishes.
	for {
		s.mu.Lock()
		done, busy := s.active[key]
		if !busy {
			done = make(chan struct{})
			s.active[key] = done
			s.mu.Unlock()
			defer func() {
				s.mu.Lock()
				delete(s.active, key)
				close(done)
				s.mu.Unlock()
			}()
			break
		}
		s.mu.Unlock()
		select {
		case <-ctx.Done():
			return "", context.Cause(ctx)
		case <-done:
		}
		if err := workguard.Check(ctx); err != nil {
			return "", err
		}
	}
	text, found, err := s.store.Load(ctx, key)
	if err != nil {
		return "", s.fail("load", err)
	}
	if err := workguard.Check(ctx); err != nil {
		return "", err
	}
	if found {
		if reusable == nil || reusable(text) {
			return text, nil
		}
		if err := s.store.Delete(ctx, key); err != nil {
			return "", s.fail("discard invalid result", err)
		}
	}
	if err := workguard.Check(ctx); err != nil {
		return "", err
	}
	text, err = generate()
	if err != nil {
		return text, err
	}
	if err := workguard.Check(ctx); err != nil {
		return "", err
	}
	if strings.TrimSpace(text) == "" || (reusable != nil && !reusable(text)) {
		return text, nil
	}
	if err := s.store.Save(ctx, key, text); err != nil {
		return "", s.fail("save", err)
	}
	if err := workguard.Check(ctx); err != nil {
		return "", err
	}
	return text, nil
}

// JSON checkpoints structured inputs or validated intermediate results before
// later side effects can change the data used to construct them on a retry.
func JSON[T any](ctx context.Context, key string, generate func() (T, error)) (T, error) {
	s, bound := ctx.Value(contextKey{}).(*state)
	if !bound {
		return generate()
	}
	raw, err := Do(ctx, key, func() (string, error) {
		value, err := generate()
		if err != nil {
			return "", err
		}
		encoded, err := json.Marshal(value)
		return string(encoded), err
	})
	var value T
	if err != nil {
		return value, err
	}
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		return value, s.fail("decode "+key, err)
	}
	return value, nil
}
