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
	Turn    int    `json:"turn"`
	Role    string `json:"role"` // "user" | "assistant"
	Text    string `json:"text"`
	Refined bool   `json:"refined"`
	// CreatedAt is when this turn was first saved (unix seconds) — set once,
	// at insert, and never touched by a later upsert (e.g. refined_transcript
	// overwriting text). Lets the frontend group a replayed transcript into
	// date-divided days and show a per-message time.
	CreatedAt int64 `json:"createdAt"`
	// Source is how the learner produced this turn — protocol.SourceVoice or
	// protocol.SourceText. Empty for assistant turns, which are always
	// generated rather than input by the learner.
	Source     string               `json:"source,omitempty"`
	Correction *protocol.Correction `json:"correction,omitempty"`
	// Translation is a native-language translation of Text, set for both
	// user and assistant turns (see SaveTranslation). Kept separate from
	// Correction, which is user-only and scoped to grammar feedback.
	Translation string `json:"translation,omitempty"`
	// Meta is an open-ended bag for signals beyond the transcript itself —
	// e.g. a future emotion/tone classifier's output. Nothing populates it
	// yet; it exists so that lands as a data addition, not another schema
	// migration. Opaque on purpose: store doesn't know or care what keys it
	// contains.
	Meta json.RawMessage `json:"meta,omitempty"`
	// ReplyStatus is this turn's reply-job status ("pending"/"processing"/
	// "done"/"failed"), populated only for an assistant turn reserved via
	// ReserveAssistantTurn. Empty for a user turn, and for an assistant turn
	// saved before this feature existed (SaveTurn, not ReserveAssistantTurn
	// + CompleteAssistantTurn) — both cases mean "nothing to poll for", so
	// the frontend doesn't need to tell them apart.
	ReplyStatus string `json:"replyStatus,omitempty"`
	// CorrectionStatus mirrors ReplyStatus for the grammar-correction job on
	// a user turn, populated only when it was reserved via
	// ReserveCorrectionJob. In particular JobStatusFailed here — as opposed
	// to Correction present with an empty Issues slice — is what lets the
	// frontend tell "the analysis pass ran and found nothing to flag" apart
	// from "the analysis pass itself errored", instead of both looking like
	// a turn that just never got a correction (see GrammarControl in
	// apps/web/src/App.tsx). Empty for an assistant turn, and for a user
	// turn saved before this feature existed.
	CorrectionStatus string `json:"correctionStatus,omitempty"`
}

// Job status values for the (user_id, session_id, turn, kind) rows tracked
// alongside a turn's content — see ReserveAssistantTurn/CompleteAssistantTurn/
// FailJob/JobStatus, and internal/asyncjob for the queue that drives a job
// through these states.
const (
	JobStatusPending = "pending"
	JobStatusDone    = "done"
	JobStatusFailed  = "failed"
)

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

	// GetInterlocutorStyle returns userID's saved free-text preference for
	// how the AI conversation partner should talk to them (e.g. "ask
	// interview-style questions", "sound like a professional") — "" if never
	// set. It's global to the user, not scoped to one session: see
	// pipeline.BuildSystemPrompt, which layers it onto the chat persona when
	// a session is created (transport.Handler.ServeHTTP).
	GetInterlocutorStyle(ctx context.Context, userID string) (string, error)
	// SaveInterlocutorStyle persists userID's conversation-style preference,
	// replacing any previous value. An empty style clears it back to the
	// default persona.
	SaveInterlocutorStyle(ctx context.Context, userID, style string) error

	// SaveTurn upserts one message into a session's transcript, creating the
	// session's row (and its title, derived from turn 1's text) on first
	// write. source is protocol.SourceVoice/SourceText for a user turn, or ""
	// for an assistant turn.
	SaveTurn(ctx context.Context, userID, sessionID string, turn int, role, text string, refined bool, source string) error
	// SaveCorrection attaches grammar/vocabulary feedback to an existing
	// user turn and, if a correction job was reserved for it (see
	// ReserveCorrectionJob), marks that job done in the same transaction —
	// so a poller never observes a "done" status before the correction text
	// it belongs to is actually visible. A no-op on the turn write if that
	// turn hasn't been saved yet; a no-op on the job write if none was ever
	// reserved (e.g. a caller that predates ReserveCorrectionJob or a test
	// double).
	SaveCorrection(ctx context.Context, userID, sessionID string, turn int, c protocol.Correction) error
	// ReserveCorrectionJob writes a "pending" correction-job row for
	// (userID, sessionID, turn), before that turn's correction job is even
	// enqueued — mirrors ReserveAssistantTurn, minus the placeholder-text
	// insert (the user turn this job corrects has already been saved by the
	// time correction ever runs). A no-op if this exact job was already
	// reserved (e.g. a race between two connections, or a retry).
	ReserveCorrectionJob(ctx context.Context, userID, sessionID string, turn int) error
	// SaveTranslation attaches a native-language translation to an existing
	// turn. role disambiguates a user turn from its paired assistant turn,
	// since both share the same turn number. A no-op if that turn hasn't
	// been saved yet.
	SaveTranslation(ctx context.Context, userID, sessionID string, turn int, role, translation string) error

	// ReserveAssistantTurn writes a placeholder assistant-turn row (empty
	// text) plus a "pending" reply-job row for (userID, sessionID, turn),
	// before that turn's reply job is even enqueued — so a poller (see
	// JobStatus, and Turn.ReplyStatus in SessionDetail) has something to
	// observe immediately, and durably records that this turn's reply is
	// in flight even if the enqueuing process dies before the job runs. A
	// no-op if this exact reply job was already reserved (e.g. a race
	// between two connections, or a retry) — it never clobbers an
	// already-completed row's text.
	ReserveAssistantTurn(ctx context.Context, userID, sessionID string, turn int) error
	// CompleteAssistantTurn writes the finished assistant reply text and
	// marks (userID, sessionID, turn)'s reply job "done", atomically. Called
	// by whichever replica's worker actually ran the reply job — see
	// internal/pipeline.ReplyJobHandler — regardless of whether that's the
	// same replica the learner's WebSocket connection is still on.
	CompleteAssistantTurn(ctx context.Context, userID, sessionID string, turn int, text string) error
	// FailJob marks (userID, sessionID, turn, kind)'s job "failed" with
	// errMsg, for observability. This does not itself stop retries —
	// internal/asyncjob's stale-claim reaper retries a job regardless of
	// this status — it only records the most recent attempt's outcome.
	FailJob(ctx context.Context, userID, sessionID string, turn int, kind, errMsg string) error
	// JobStatus returns (userID, sessionID, turn, kind)'s current status
	// (JobStatusPending/"processing"/JobStatusDone/JobStatusFailed), or ""
	// if no such job was ever reserved (e.g. a turn from before "reply" or
	// "correction" job tracking existed, or a kind that isn't tracked this
	// way at all).
	JobStatus(ctx context.Context, userID, sessionID string, turn int, kind string) (string, error)

	// LastTurn returns the highest turn number already persisted for a
	// session (0 if none), so a resumed session can continue numbering
	// turns from where the transcript left off instead of restarting at 0
	// and colliding with — silently overwriting — turns already saved
	// under those same numbers. See internal/session.Session.Seed.
	LastTurn(ctx context.Context, userID, sessionID string) (int, error)

	// SaveGeneratedTitle sets a session's title to an LLM-generated one,
	// exactly once — a no-op if this session's title was already
	// auto-generated (see internal/transport, which triggers this once per
	// WS connection's first turn; a reconnect resets its own turn counter,
	// so this guard is what actually keeps the title from being
	// regenerated and flapping on every reconnect). Also a no-op if the
	// session row doesn't exist yet.
	SaveGeneratedTitle(ctx context.Context, userID, sessionID, title string) error

	// ListSessions returns userID's chat rooms, most recently active first.
	// Only sessions with at least one saved turn appear (see SaveTurn).
	ListSessions(ctx context.Context, userID string) ([]SessionMeta, error)
	// SessionDetail returns one session's full transcript, in turn order.
	// Returns ErrNotFound if it doesn't exist or belongs to a different user.
	SessionDetail(ctx context.Context, userID, sessionID string) (SessionMeta, []Turn, error)

	// DeleteSession removes a session and its full transcript. A no-op (nil
	// error) if sessionID doesn't exist or belongs to a different user — same
	// indistinguishable-from-missing contract as Load.
	DeleteSession(ctx context.Context, userID, sessionID string) error

	Close() error
}
