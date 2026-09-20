// Package workguard keeps background work tied to its persisted owner, while
// allowing it to outlive the HTTP request or WebSocket that started it.
package workguard

import (
	"context"
	"errors"
	"time"
)

var ErrDeleted = errors.New("work owner deleted")

type Source interface {
	WorkExists(context.Context, string, string) (bool, error)
}

type checkKey struct{}
type commitKey struct{}
type commitFunc func(context.Context, func(context.Context) error) error

type Committer interface {
	WorkCommit(context.Context, string, string, func(context.Context) error) error
}

// Commit orders a short derived write (e.g. a profile or captured word) before
// deletion, or rejects it when deletion won. Model calls must stay outside it.
func Commit(ctx context.Context, work func(context.Context) error) error {
	if err := Check(ctx); err != nil {
		return err
	}
	if commit, ok := ctx.Value(commitKey{}).(commitFunc); ok {
		return commit(ctx, work)
	}
	return work(ctx)
}

type CheckFunc func(context.Context) error

// Bind preserves earlier guards so derived work can depend on multiple owners.
func Bind(ctx context.Context, check CheckFunc) context.Context {
	previous, _ := ctx.Value(checkKey{}).(CheckFunc)
	return context.WithValue(ctx, checkKey{}, CheckFunc(func(ctx context.Context) error {
		if previous != nil {
			if err := previous(ctx); err != nil {
				return err
			}
		}
		return check(ctx)
	}))
}

// BindStore uses a narrow primary-read capability. Stores without this
// capability retain their existing behavior (e.g. in-memory test doubles).
func BindStore(ctx context.Context, source any, userID, id string) context.Context {
	if s, ok := source.(Source); ok {
		if fence, ok := source.(Committer); ok {
			previous, _ := ctx.Value(commitKey{}).(commitFunc)
			ctx = context.WithValue(ctx, commitKey{}, commitFunc(func(ctx context.Context, work func(context.Context) error) error {
				next := func(ctx context.Context) error { return fence.WorkCommit(ctx, userID, id, work) }
				if previous != nil {
					return previous(ctx, next)
				}
				return next(ctx)
			}))
		}
		return Bind(ctx, func(ctx context.Context) error {
			exists, err := s.WorkExists(ctx, userID, id)
			if err != nil {
				return err
			}
			if !exists {
				return ErrDeleted
			}
			return nil
		})
	}
	return ctx
}

func Check(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		if cause := context.Cause(ctx); cause != nil {
			return cause
		}
		return err
	}
	if check, ok := ctx.Value(checkKey{}).(CheckFunc); ok {
		return check(ctx)
	}
	return nil
}

// Run rechecks at both boundaries and polls the primary during slow I/O.
// A guard failure cancels the actual request, including on another replica.
// The final check discards responses from clients that ignore cancellation.
func Run(ctx context.Context, work func(context.Context) error) error {
	if err := Check(ctx); err != nil {
		return err
	}
	if _, ok := ctx.Value(checkKey{}).(CheckFunc); !ok {
		return work(ctx)
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	return run(ctx, ticker.C, work)
}

func run(ctx context.Context, ticks <-chan time.Time, work func(context.Context) error) error {
	ctx, cancel := context.WithCancelCause(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticks:
				if err := Check(ctx); err != nil {
					cancel(err)
					return
				}
			}
		}
	}()
	err := work(ctx)
	if checkErr := Check(ctx); checkErr != nil {
		err = checkErr
	}
	cancel(nil)
	<-done
	return err
}
