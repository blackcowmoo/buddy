// Package store persists each learner's long-term memory so it survives
// reconnects and server restarts. A Profile is small by design: a compact
// running summary plus a short verbatim window (see internal/session, which
// compacts the window down to this shape as it grows).
package store

import (
	"context"

	"buddy/server/internal/llm"
)

// Profile is one user's persistent memory.
type Profile struct {
	Summary string
	Recent  []llm.Message
}

// Store persists Profiles keyed by an opaque user ID. The ID's origin is not
// Store's concern — today it comes from internal/identity's anonymous cookie;
// swapping in real auth (e.g. a Google OAuth `sub` claim) later needs no
// change here.
type Store interface {
	// Load returns the zero Profile (not an error) if userID has no record yet.
	Load(ctx context.Context, userID string) (Profile, error)
	Save(ctx context.Context, userID string, p Profile) error
	Close() error
}
