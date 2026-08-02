package transport

import (
	"context"
	"time"

	"buddy/server/internal/llm"
	"buddy/server/internal/session"
)

// titleTimeout bounds Handler.generateTitle's LLM call — its own budget,
// not the connection's ctx, since a barge-in or disconnect right after
// the first reply must not cut short the one-shot title generation for
// that room (mirrors audioSaveTimeout in ws.go). Matches llm.OpenAI's own
// request timeout: a locally hosted model can take far longer than a
// hosted API to answer even this one-shot call, and a tighter budget
// here would just fail it early and force a retry on an already-slow
// server.
const titleTimeout = 24 * time.Hour

// generateTitle asks the pipeline's LLM for a proper chat-room title from
// the conversation so far — on turn 1 this replaces the raw-text-truncation
// placeholder store.MySQLStore.SaveTurn sets, and every
// TitleRegenerateEveryNTurns turns after that it re-titles the room from the
// fuller transcript. Runs entirely off the live turn: emit fires this via
// `go` so the learner's reply is never delayed, and it uses
// context.Background() (bounded by titleTimeout, not the connection's ctx)
// so a disconnect right after the reply doesn't cut it short — same
// reasoning as backupAudio below.
//
// sess.Export()'s recent window reliably includes every turn up to and
// including this turn's user message (session.Session.AppendUser runs
// synchronously before the pipeline's async work starts), but NOT this
// turn's assistant reply — pipeline.HandleText/HandleUtterance only calls
// sess.AppendAssistant right after emit() returns, racing this goroutine.
// That's why assistantText is passed in explicitly (as ev.Text) and appended
// here rather than re-read from sess.
//
// ev.Turn == 1 normally happens once per session's lifetime — session.New's
// turn counter is seeded from store.Store.LastTurn on every connect (see
// session.Session.Seed), so a reconnect resumes numbering rather than
// restarting at 0. It can still recur (e.g. a race between two connections
// both seeing the same LastTurn before either has saved turn 1), so this
// isn't relied on for correctness: titleDedupeKey includes the turn number,
// so a repeat call for the same turn just regenerates an equivalent title
// rather than flapping between two different ones.
func (h *Handler) generateTitle(userID, sessionID string, sess *session.Session, turn int, assistantText string) {
	_, recent := sess.Export()
	transcript := make([]llm.Message, 0, len(recent)+1)
	transcript = append(transcript, recent...)
	transcript = append(transcript, llm.Message{Role: llm.RoleAssistant, Content: assistantText})
	hasUserTurn := false
	for _, m := range transcript {
		if m.Role == llm.RoleUser {
			hasUserTurn = true
			break
		}
	}
	if !hasUserTurn {
		return
	}
	// ctx is titleTimeout-bounded (its own budget, not the connection's —
	// see that const's doc comment); RunTitle's queue-backed TitleHook path
	// ignores it and uses context.Background() internally instead (see
	// NewTitleHook), the same "hook always keeps making progress past a
	// disconnect" convention as NewReplyHook/NewCorrectHook. save is only
	// consulted on the direct (no-hook) fallback — see RunTitle's doc
	// comment for why the hook path persists on its own.
	ctx, cancel := context.WithTimeout(context.Background(), titleTimeout)
	defer cancel()
	h.pipe.RunTitle(ctx, userID, sessionID, turn, transcript, func(title string) error {
		return h.store.SaveGeneratedTitle(context.Background(), userID, sessionID, title)
	})
}
