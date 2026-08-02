package pipeline

import (
	"context"
	"log"
	"strings"
	"sync/atomic"

	"buddy/server/internal/llm"
	"buddy/server/internal/protocol"
	"buddy/server/internal/session"
)

// reply streams the assistant response and records it in the session.
func (p *Pipeline) reply(ctx context.Context, userID, sessionID string, sess *session.Session, turn int, emit Emit) {
	msgs := sess.Snapshot()
	p.runReply(ctx, userID, sessionID, sess, turn, msgs, fallbackReply(msgs), emit)
}

// runReply is reply()/StartConversation()'s shared body: either calls the
// chat model directly (the default, when ReplyHook is nil) or delegates to
// ReplyHook — see that field's doc comment for why a hook may finish
// (or never even start) this exact call synchronously, and what onDone
// firing later, asynchronously, out from under this ctx means for a caller.
func (p *Pipeline) runReply(ctx context.Context, userID, sessionID string, sess *session.Session, turn int, msgs []llm.Message, fallback string, emit Emit) {
	onToken := func(tok string) {
		emit(protocol.ServerEvent{Type: protocol.EvAssistantDelta, Turn: turn, Text: tok})
	}
	// onDone is the bookkeeping that must happen exactly once, however (and
	// whenever) the reply text was actually produced: tell the learner it's
	// finished, fold it into this connection's in-memory context so
	// subsequent turns see it, and kick off its translation/compaction.
	// ReplyHook may call this synchronously (fast path, this connection ran
	// the job itself) or much later from a poll loop (this connection
	// didn't) — either way it's the same completion, so it's handled once,
	// here, rather than duplicated in every hook implementation.
	onDone := func(full string) {
		emit(protocol.ServerEvent{Type: protocol.EvAssistantDone, Turn: turn, Text: full})
		sess.AppendAssistant(full)
		if strings.TrimSpace(full) != "" {
			// context.WithoutCancel: the reply already streamed to the learner,
			// so its translation is a self-contained piece of work with
			// nothing left to race — a barge-in or disconnect must not lose
			// it, same reasoning as backupAudio/persistEvent detaching from ctx.
			go p.translateAssistant(context.WithoutCancel(ctx), userID, sessionID, turn, full, emit)
		}
		go p.compact(sess) // background: fold old turns into the long-term summary
	}
	if p.ReplyHook != nil {
		p.ReplyHook(ctx, userID, sessionID, turn, msgs, fallback, onToken, onDone)
		return
	}
	full, _ := p.GenerateReply(ctx, msgs, fallback, onToken)
	// Barged in (user spoke again) or disconnected: drop this turn's tail,
	// keep history clean, rather than appending/emitting a reply nobody will
	// ever see. Only applies to this direct, in-process path — once a
	// ReplyHook is handling durability, ctx no longer governs whether the
	// reply completes (see that field's doc comment).
	if ctx.Err() != nil {
		return
	}
	onDone(full)
}

// GenerateReply calls the chat model once for msgs, streaming tokens via
// onToken as they arrive, and returns the full text — substituting fallback
// (and still calling onToken with it) if the call failed outright with
// nothing usable. Exported so transport's queue-backed ReplyHook
// implementation (running this same call on whichever replica ends up
// executing the job — see internal/asyncjob) can reuse the exact same chat
// call reply()/StartConversation() use directly by default. Has no
// dependency on session/emit — see runReply for the bookkeeping layered on
// top when it's called in-process.
func (p *Pipeline) GenerateReply(ctx context.Context, msgs []llm.Message, fallback string, onToken func(string)) (full string, err error) {
	atomic.AddInt32(&p.chatActive, 1)
	full, err = p.LLM.ChatStream(ctx, p.ChatModel, msgs, onToken)
	atomic.AddInt32(&p.chatActive, -1)
	// A caller whose ctx is already done (e.g. runReply's direct path, on a
	// barge-in or disconnect) is about to drop this result entirely — don't
	// bother substituting/emitting a fallback nobody will ever see. A
	// caller running on context.Background() (every ReplyHook path) never
	// hits this, so the fallback substitution below still always applies
	// there.
	if ctx.Err() != nil {
		return full, err
	}
	if err != nil {
		log.Printf("chat: %v", err)
		if strings.TrimSpace(full) == "" {
			full = fallback
			onToken(full)
		}
	}
	return full, err
}

func fallbackReply(msgs []llm.Message) string {
	last := ""
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == llm.RoleUser {
			last = msgs[i].Content
			break
		}
	}
	return "(LLM offline) I heard: \"" + last + "\". Tell me more!"
}
