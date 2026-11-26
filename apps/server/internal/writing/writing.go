// Package writing persists learner-specific writing prompts and the answer
// check results shown on the Today's Sentence page.
package writing

import (
	"context"
	"time"

	"buddy/server/internal/protocol"
)

const (
	StatusPending = "pending"
	StatusDone    = "done"
	StatusFailed  = "failed"
)

// Answer check statuses. "" means no answer is currently being checked and
// no result is waiting (the initial state, and the state after a failed
// start clears a pending check).
const (
	CheckPending = "pending"
	CheckDone    = "done"
	CheckFailed  = "failed"
)

type Prompt struct {
	ID        string
	UserID    string
	Korean    string
	Status    string
	CreatedAt time.Time

	// Last submitted answer and the asynchronously produced check result
	// for it. The answer persists so returning to the page restores it;
	// CheckStatus tracks the detached check job for it.
	Answer      string
	CheckStatus string // "" | pending | done | failed
	Corrected   string
	Issues      []protocol.Issue
}

type Store interface {
	Create(ctx context.Context, userID string) (Prompt, error)
	List(ctx context.Context, userID string) ([]Prompt, error)
	Get(ctx context.Context, userID, id string) (Prompt, error)
	Delete(ctx context.Context, userID, id string) error
	Complete(ctx context.Context, id, korean string) error
	Fail(ctx context.Context, id string) error
	// StartCheck records the answer and marks its check pending in one
	// write, so a submitted answer is never stored without a job bound to
	// it. It returns whether the prompt still exists.
	StartCheck(ctx context.Context, userID, id, answer string) (bool, error)
	// CompleteCheck and FailCheck write the result only while the given
	// answer is still the pending one; a result for a superseded answer
	// is dropped rather than overwriting the newer check.
	CompleteCheck(ctx context.Context, id, answer, corrected string, issues []protocol.Issue) (bool, error)
	FailCheck(ctx context.Context, id, answer string) (bool, error)
	// Recheck answers the "check this answer again" request: when an
	// answer is stored and no check is already running it drops the old
	// result and marks the check pending in one write, so exactly one
	// re-dispatch can claim it. It reports false when nothing started
	// (unknown prompt, no stored answer, or a check already pending).
	Recheck(ctx context.Context, userID, id string) (bool, error)
	Close() error
}
