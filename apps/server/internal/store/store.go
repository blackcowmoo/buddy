// Package store persists each learner's conversations. A user can have many
// sessions (chat rooms, in the frontend's terms); each session carries its
// own compacting long-term memory (Profile, see internal/session) plus a
// full, uncompacted transcript (Turn) used to replay the session and review
// past corrections. Everything persisted here is text — audio is never
// written to the store; it lives only in memory for the duration of one STT
// call (see internal/stt) and is discarded immediately after.
package store

import (
	"context"
	"encoding/json"
	"errors"

	"buddy/server/internal/llm"
	"buddy/server/internal/protocol"
)

// ErrNotFound is returned by SessionDetail when no session matches — either
// it doesn't exist, or it belongs to a different user. The two cases are
// deliberately indistinguishable to the caller, so an API response can't be
// used to probe for the existence of someone else's session.
var ErrNotFound = errors.New("store: not found")

// Profile is one session's persistent LLM-context memory: a compact running
// summary plus a short verbatim window (see internal/session, which compacts
// the window down to this shape as it grows).
type Profile struct {
	Summary string
	Recent  []llm.Message
}

// SessionMeta describes one chat room for listing/display.
type SessionMeta struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	CreatedAt int64  `json:"createdAt"`
	UpdatedAt int64  `json:"updatedAt"`
}

// Turn is one persisted message in a session's full transcript — the source
// of truth for replaying a session and reviewing past corrections. Unlike
// Profile.Recent, this is never compacted or dropped as the conversation
// grows.
type Turn struct {
	Turn       int                  `json:"turn"`
	Role       string               `json:"role"` // "user" | "assistant"
	Text       string               `json:"text"`
	Refined    bool                 `json:"refined"`
	Correction *protocol.Correction `json:"correction,omitempty"`
	// Meta is an open-ended bag for signals beyond the transcript itself —
	// e.g. a future emotion/tone classifier's output. Nothing populates it
	// yet; it exists so that lands as a data addition, not another schema
	// migration. Opaque on purpose: store doesn't know or care what keys it
	// contains.
	Meta json.RawMessage `json:"meta,omitempty"`
}

// Store persists everything keyed by an opaque user ID (see
// internal/identity) and, within a user, an opaque session ID (one per chat
// room). Every method takes both IDs together so one user's data is never
// reachable through another user's request, even if a session ID leaks or is
// guessed — see the composite primary keys in internal/store/mysql.go.
type Store interface {
	// Load returns the zero Profile (not an error) if the session has no
	// record yet — e.g. a brand-new session ID, or one that belongs to a
	// different user (never surfaced as such; see ErrNotFound).
	Load(ctx context.Context, userID, sessionID string) (Profile, error)
	Save(ctx context.Context, userID, sessionID string, p Profile) error

	// SaveTurn upserts one message into a session's transcript, creating the
	// session's row (and its title, derived from turn 1's text) on first
	// write.
	SaveTurn(ctx context.Context, userID, sessionID string, turn int, role, text string, refined bool) error
	// SaveCorrection attaches grammar/vocabulary feedback to an existing
	// user turn. A no-op if that turn hasn't been saved yet.
	SaveCorrection(ctx context.Context, userID, sessionID string, turn int, c protocol.Correction) error
	// DeleteTurns clears a session's transcript (used by the "reset
	// conversation" action) without touching its long-term summary or title.
	DeleteTurns(ctx context.Context, userID, sessionID string) error

	// ListSessions returns userID's chat rooms, most recently active first.
	// Only sessions with at least one saved turn appear (see SaveTurn).
	ListSessions(ctx context.Context, userID string) ([]SessionMeta, error)
	// SessionDetail returns one session's full transcript, in turn order.
	// Returns ErrNotFound if it doesn't exist or belongs to a different user.
	SessionDetail(ctx context.Context, userID, sessionID string) (SessionMeta, []Turn, error)

	Close() error
}
