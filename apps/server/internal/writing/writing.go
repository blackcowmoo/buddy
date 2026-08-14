// Package writing persists learner-specific writing prompts.
package writing

import (
	"context"
	"time"
)

const (
	StatusPending = "pending"
	StatusDone    = "done"
	StatusFailed  = "failed"
)

type Prompt struct {
	ID        string
	UserID    string
	Korean    string
	Status    string
	CreatedAt time.Time
}

type Store interface {
	Create(ctx context.Context, userID string) (Prompt, error)
	List(ctx context.Context, userID string) ([]Prompt, error)
	Get(ctx context.Context, userID, id string) (Prompt, error)
	Delete(ctx context.Context, userID, id string) error
	Complete(ctx context.Context, id, korean string) error
	Fail(ctx context.Context, id string) error
	Close() error
}
