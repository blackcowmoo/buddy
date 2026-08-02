// Package pipeline orchestrates one conversational turn across two tracks:
//
//	FAST   : STT ensemble, quick-synthesized -> streamed LLM reply (low
//	         latency, what the user hears)
//	REFINE : the SAME STT candidates, Judge-synthesized with more care ->
//	         grammar/context correction (background, more accurate)
//
// Every configured STT engine is called concurrently on each utterance (see
// transcribe()); disagreements between them (e.g. STT mishearing a
// homophone) are reconciled by an LLM using the conversation for context,
// not by picking one engine as "the" answer. FAST does this quickly with the
// chat model so the reply doesn't stall; REFINE reruns the same
// reconciliation with Judge in the background — a stronger model, same
// inputs — as a second, more careful opinion, without re-running STT. The
// two tracks share the session so REFINE can upgrade the last user turn if
// it lands on a better answer.
package pipeline

import (
	"context"
	"log"
	"strings"
	"sync"

	"buddy/server/internal/llm"
	"buddy/server/internal/protocol"
	"buddy/server/internal/session"
	"buddy/server/internal/stt"
)

// Emit delivers an event to the client. It is safe for concurrent use — the
// transport layer serializes writes.
type Emit func(protocol.ServerEvent)

const basePersonaPrompt = `You are Buddy, a warm, encouraging English conversation partner.
Keep the conversation flowing naturally: reply in 1-3 short spoken-style sentences,
ask a follow-up question, and match the learner's level. Do NOT correct grammar
inline — corrections are handled separately. Never mention that you are an AI.`

// BuildSystemPrompt returns the chat persona's system prompt, layering the
// learner's own free-text conversation-style preference (saved via
// GET/PUT /api/settings, e.g. "ask interview-style questions", "respond like
// a professional") and their persistent cross-session learner profile (see
// UpdateLearnerProfile — recurring mistakes/interests/proficiency, folded in
// from every conversation the learner has ever ended, not just this one) on
// top of the base persona. An empty/blank style or learnerProfile leaves the
// corresponding paragraph out. Called once per session, at creation
// (transport.Handler.ServeHTTP) — a session's system prompt doesn't change
// mid-conversation even if the learner edits the setting or ends another
// session elsewhere.
func BuildSystemPrompt(style, learnerProfile string) string {
	prompt := basePersonaPrompt
	if style = strings.TrimSpace(style); style != "" {
		prompt += "\n\nThe learner has also asked you to follow this conversation style: " + style
	}
	if learnerProfile = strings.TrimSpace(learnerProfile); learnerProfile != "" {
		prompt += "\n\nWhat you know about this learner from past conversations (for context only, never mention this explicitly): " + learnerProfile
	}
	return prompt
}

// Candidate is one ensemble member consulted during REFINE-track analysis
// (grammar correction, context compaction): a model and the endpoint that
// serves it. See Pipeline.Analysis and analyze().
type Candidate struct {
	LLM   llm.Client
	Model string
}

type Pipeline struct {
	// STT: every configured engine, called concurrently on each utterance
	// (transcribe()) — an ensemble, not a fast/slow pair. A lone engine (or
	// the zero-setup mock default) just skips the reconciliation LLM call.
	STT []stt.Recognizer

	// LLM/ChatModel: FAST track's streamed reply — one model, low latency.
	// Also does the FAST track's quick STT-candidate reconciliation.
	LLM       llm.Client
	ChatModel string

	// Analysis: REFINE track's grammar-correction/compaction pass. Every
	// candidate is asked concurrently (analyze()); a lone candidate is used
	// directly, and two or more are synthesized by Judge. This is how
	// multiple local models (e.g. several checkpoints behind llama.cpp) get
	// combined into one higher-confidence result instead of picking just one.
	Analysis []Candidate
	// Judge synthesizes the Analysis ensemble's outputs into the single
	// result analyze() returns. Unused (and may be nil) when len(Analysis)<=1.
	// Also does the REFINE track's STT-candidate reconciliation (refine()) —
	// a second, more careful opinion than the FAST track's chat-model pass.
	Judge      llm.Client
	JudgeModel string

	// FeedbackLang is the learner's native language for correction
	// explanations (e.g. "ko"). The corrected sentence stays in English.
	FeedbackLang string
	// MaxHistoryMessages caps the verbatim window kept in session.Session
	// before the oldest half is folded into its long-term summary. See
	// compact() and session.PeekOldestForCompaction.
	MaxHistoryMessages int

	// chatActive counts in-flight chat replies (reply/StartConversation's
	// ChatStream calls). Read by acquireTranslationSlot so a new translation
	// call can wait for a gap in chat activity instead of contending with it.
	chatActive int32 // atomic

	// translationSem caps translateAssistant/TranslateWithContext to one
	// call in flight at a time, process-wide (shared by live per-turn
	// translation and internal/backfill's worker, since both hold the same
	// *Pipeline). Lazily created so Pipeline stays usable as a zero-value
	// struct literal (see cmd/server/main.go).
	translationSemOnce sync.Once
	translationSem     chan struct{}

	// ReplyHook, if set, replaces reply()/StartConversation()'s direct
	// "call the chat model in this goroutine" behavior with a durable,
	// queue-backed one — see transport.NewReplyHook, which builds this from
	// internal/asyncjob so a turn's reply survives both the learner's
	// connection disconnecting and this replica dying mid-generation. msgs
	// is the exact conversation snapshot to reply to (already including
	// StartConversation's extra opening-turn system message, where
	// applicable) and fallback is the text to fall back to if the chat
	// model call fails outright — both computed by the caller so the hook
	// doesn't need to know which of reply()/StartConversation() invoked it.
	// onToken/onDone mirror Emit's shape without requiring transport's hook
	// to import internal/protocol: onToken fires per streamed token (only
	// when this exact call ends up running the LLM itself — see the
	// fast-path/poll-fallback split in transport.NewReplyHook), onDone
	// fires exactly once with the finished text, however it was produced.
	// Left nil (the zero-value default), reply()/StartConversation() call
	// the chat model directly exactly as before this existed — entirely
	// optional, gated on whether transport wired one up (itself gated on
	// Redis being configured), the same convention as every other optional
	// feature in this codebase.
	ReplyHook ReplyHook

	// CorrectHook/TranslateHook are correct()/translateAssistant()'s
	// equivalents of ReplyHook — see that field's doc comment for the
	// shared rationale (durability across disconnects and replica deaths,
	// entirely optional, nil by default). Unlike replies, a correction or
	// translation job that completes on a different replica than the
	// learner's own has no live connection to stream a result to at all,
	// so there's no analog of ReplyHook's poll-fallback here: the
	// frontend's existing pollMissingFeedback (see apps/web/src/App.tsx)
	// already re-fetches and picks up a correction/translation that landed
	// after the fact, since both are just columns on a turn's row, not a
	// live-only event.
	CorrectHook   CorrectHook
	TranslateHook TranslateHook

	// TitleHook, if set, replaces RunTitle's direct "call the LLM in this
	// goroutine, then save" behavior with a durable, queue-backed one — see
	// transport.NewTitleHook. Same nil-by-default/optional convention as
	// ReplyHook. Unlike CorrectHook/TranslateHook there's no onResult: a
	// title has no live-connection UX to update (no ServerEvent for it, see
	// transport.persistEvent), so the hook is trusted to persist the result
	// itself (see transport.TitleJobHandler) rather than reporting it back.
	TitleHook TitleHook
}

// ReplyHook is Pipeline.ReplyHook's type — see that field's doc comment.
type ReplyHook func(ctx context.Context, userID, sessionID string, turn int, msgs []llm.Message, fallback string, onToken func(string), onDone func(string))

// CorrectHook is Pipeline.CorrectHook's type. Exactly one of onResult/
// onFailure fires, however (and on whichever replica) the job settles:
// onResult delivers the parsed analysis result, onFailure signals that the
// analysis pass itself errored (LLM call failed, or its output didn't
// parse) — see correct()'s doc comment for why the live connection needs to
// hear about a failure too, not just a durably-persisted job status.
type CorrectHook func(ctx context.Context, userID, sessionID string, turn int, text, contextMsg string, onResult func(corrected string, issues []protocol.Issue, translation string), onFailure func())

// TranslateHook is Pipeline.TranslateHook's type. onResult delivers the
// finished translation exactly once, however (and on whichever replica) it
// was produced.
type TranslateHook func(ctx context.Context, userID, sessionID string, turn int, text string, onResult func(translation string))

// TitleHook is Pipeline.TitleHook's type — see that field's doc comment.
type TitleHook func(ctx context.Context, userID, sessionID string, turn int, transcript []llm.Message)

// STTNames returns the configured STT engines' Name()s, in registration
// order. The ensemble is fixed once Pipeline is constructed, so callers that
// just want a label (main's boot log, httpserver's /api/health) should call
// this once and reuse the result rather than recomputing it per request.
func (p *Pipeline) STTNames() []string {
	names := make([]string, len(p.STT))
	for i, r := range p.STT {
		names[i] = r.Name()
	}
	return names
}

// HandleUtterance runs STT on one utterance and hands the learner an
// editable draft (EvPendingTranscript) — it deliberately stops there. STT
// (doubly so an ensemble reconciled by an LLM, see transcribe) can produce
// text the learner never actually said; committing that straight into the
// session used to spend a correction call, a chat reply, and a TTS playback
// on every mis-hearing. Now nothing joins the conversation — no session
// append, no correct(), no reply() — until the learner reviews the draft and
// actually sends it, which comes back as an ordinary "text" ClientMsg with
// Source set to SourceVoice and is handled by HandleText exactly like typed
// input. userID/sessionID are accepted only to keep this signature parallel
// with HandleText/ws.go's call site; this function itself no longer needs
// them since it never reaches a hook that would.
func (p *Pipeline) HandleUtterance(ctx context.Context, userID, sessionID string, sess *session.Session, pcm []byte, emit Emit) {
	// Pre-utterance context: used for both STT reconciliation passes below so
	// they answer the exact same question (same inputs, different model/care
	// level), and so the unconfirmed guess never contaminates its own
	// disambiguation context.
	summary, recent := sess.Export()

	// --- FAST STT ensemble -------------------------------------------------
	userText, candidates, err := p.transcribe(ctx, summary, recent, pcm)
	if err != nil {
		emit(protocol.ServerEvent{Type: protocol.EvError, Text: "stt: " + err.Error()})
		return
	}
	if userText == "" {
		return
	}
	emit(protocol.ServerEvent{Type: protocol.EvPendingTranscript, Text: userText, Source: protocol.SourceVoice})

	// --- REFINE track: a slower, more careful reconciliation pass that may
	// upgrade the draft the learner is currently reviewing --------------
	if p.Judge == nil {
		return
	}
	refined, err := p.synthesizeTranscript(ctx, p.Judge, p.JudgeModel, summary, recent, candidates)
	if err != nil {
		log.Printf("refine: %v", err)
		return
	}
	if refined = strings.TrimSpace(refined); refined != "" && refined != userText {
		emit(protocol.ServerEvent{Type: protocol.EvPendingTranscript, Text: refined, Source: protocol.SourceVoice})
	}
}

// HandleText runs one turn from already-final text: typed input, or a voice
// utterance the learner reviewed and confirmed via a still-editable
// EvPendingTranscript draft (see HandleUtterance) — either way STT (if any)
// already happened, so this is also the only path that ever commits a voice
// turn/spends its correction+reply calls. userID/sessionID identify this
// turn for ReplyHook (see Pipeline.ReplyHook) — unused when it's nil. source
// should be protocol.SourceVoice or protocol.SourceText; anything else is
// normalized to SourceText.
func (p *Pipeline) HandleText(ctx context.Context, userID, sessionID string, sess *session.Session, text, source string, emit Emit) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	if source != protocol.SourceVoice {
		source = protocol.SourceText
	}
	turn := sess.NextTurn()
	emit(protocol.ServerEvent{Type: protocol.EvFinal, Turn: turn, Text: text, Source: source})
	// Capture the conversation as of BEFORE this turn, so the correction
	// pass can judge whether the sentence fits without the sentence itself
	// contaminating its own context (mirrors HandleUtterance).
	contextMsg := renderCorrectionContext(sess.Export())
	sess.AppendUser(text)
	// Reply first: the assistant's answer is what the learner is waiting on,
	// so it streams and fully completes before correction starts. correct()
	// is a single quick, non-streaming call, so when it used to run
	// concurrently with the streamed reply it routinely finished (and
	// rendered its translation/feedback) before the answer did — dispatching
	// it only once reply() returns keeps answer > (assistant) translation >
	// correction (feedback + this turn's translation) in that priority
	// order instead.
	p.reply(ctx, userID, sessionID, sess, turn, emit)
	// context.WithoutCancel: correction must finish and persist even if the
	// learner closes the tab or the socket drops right after sending — same
	// reasoning as backupAudio/compact using a context independent of ctx,
	// just without a fixed deadline since the LLM client already caps itself
	// (see llm.NewOpenAI's http.Client timeout).
	go p.correct(context.WithoutCancel(ctx), userID, sessionID, turn, text, contextMsg, emit) // correction only; no slow STT needed
}

// StartConversation generates the assistant's opening line for a brand-new
// chat room, so the learner isn't the one who always has to speak first —
// called once, right after EvReady, only when the connection minted a new
// session (see transport.ws, which calls this synchronously — NOT via `go` —
// specifically so it finishes appending to sess.history before the read loop
// can append the learner's own first turn; otherwise a fast client could
// race the greeting and land first). It deliberately does NOT call
// sess.NextTurn(): the emitted events carry the reserved sentinel turn 0, so
// the first real utterance still gets turn 1. transport.persistEvent writes
// turn 0 into the transcript like any other turn, and store.MySQLStore.SaveTurn
// creates the session row right then too (title placeholder'd from the
// greeting) — so even a "new chat" the learner never replies to shows up in
// ListSessions/SessionDetail and can be found and deleted, while a room the
// learner does reply to gets its title replaced by their own first message
// and keeps the greeting in its transcript instead of losing it on reload.
// The greeting also lives in the in-memory session history, so the LLM sees
// it as context, and it rides along in Profile.Recent once a real reply
// persists.
func (p *Pipeline) StartConversation(ctx context.Context, userID, sessionID string, sess *session.Session, emit Emit) {
	const openingTurn = 0
	msgs := append(sess.Snapshot(), llm.Message{Role: llm.RoleSystem, Content: openingSystemPrompt})
	p.runReply(ctx, userID, sessionID, sess, openingTurn, msgs, openingFallback, emit)
}

const openingSystemPrompt = `Start the conversation: the learner has not said anything yet. Greet them
warmly in 1-2 short spoken-style sentences appropriate for opening a brand new
conversation. Then ask ONE opening question:
  - If the learner profile above tells you something about this learner (an
    interest, an ongoing topic, something they were practicing or struggling
    with), pick ONE such detail and turn it into a natural, specific question
    about it. Vary which detail you pick each time rather than defaulting to
    the same one, and never say or imply that you're recalling stored notes.
  - Otherwise, ask an easy, varied opening question (their day, an interest,
    what they'd like to practice) — avoid always asking the same question.
Do not mention that you were told to do this.`

const openingFallback = "Hey there! Glad you're here — what would you like to talk about today?"
