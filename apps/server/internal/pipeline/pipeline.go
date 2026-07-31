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
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"

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

// fanOutOrdered runs call(0), call(1), ..., call(n-1) concurrently and
// returns their results in that same index order rather than completion
// order — both transcribe (STT engines) and analyze (LLM candidates) rely
// on "results[0]" meaning "the first configured engine/candidate" for their
// first-candidate fallback, regardless of which goroutine finishes first.
func fanOutOrdered[T any](n int, call func(i int) T) []T {
	results := make([]T, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = call(i)
		}(i)
	}
	wg.Wait()
	return results
}

// transcribe runs every configured STT engine concurrently on one utterance.
// A lone engine's result (or the zero-setup mock default) is used directly.
// Two or more candidates go to the FAST chat model to reconcile into the
// single sentence the learner most likely said, using the pre-turn
// conversation for context (see synthesizeTranscript) — if that call fails,
// the first engine's candidate is used so a flaky reconciliation never loses
// the turn. The raw candidates are returned too, so refine() can hand them to
// Judge for a second pass without re-running STT.
//
// Every engine failing outright is an error (STT is down); every engine
// succeeding but hearing silence is not — it returns ("", candidates, nil),
// same as a single quiet/empty transcript always has.
func (p *Pipeline) transcribe(ctx context.Context, summary string, recent []llm.Message, pcm []byte) (final string, candidates []string, err error) {
	if len(p.STT) == 0 {
		return "", nil, fmt.Errorf("transcribe: no STT engines configured")
	}

	type slot struct {
		text string
		err  error
	}
	slots := fanOutOrdered(len(p.STT), func(i int) slot {
		rec := p.STT[i]
		res, err := rec.Transcribe(ctx, pcm)
		if err != nil {
			log.Printf("transcribe: %s: %v", rec.Name(), err)
			return slot{err: err}
		}
		return slot{text: strings.TrimSpace(res.Text)}
	})

	succeeded := 0
	var errs []error
	for _, s := range slots {
		if s.err != nil {
			errs = append(errs, s.err)
			continue
		}
		succeeded++
		if s.text != "" {
			candidates = append(candidates, s.text)
		}
	}
	if succeeded == 0 {
		return "", nil, fmt.Errorf("transcribe: every STT engine failed: %w", errors.Join(errs...))
	}
	if len(candidates) == 0 {
		return "", nil, nil // every engine heard silence — not an error
	}
	if len(candidates) == 1 {
		return candidates[0], candidates, nil
	}

	text, serr := p.synthesizeTranscript(ctx, p.LLM, p.ChatModel, summary, recent, candidates)
	if serr != nil {
		log.Printf("transcribe: reconcile: %v; falling back to first candidate", serr)
		return candidates[0], candidates, nil
	}
	if text = strings.TrimSpace(text); text == "" {
		return candidates[0], candidates, nil
	}
	return text, candidates, nil
}

// synthesizeTranscript asks an LLM to reconcile one or more STT engines'
// independent (and possibly disagreeing) transcriptions of the same
// utterance into the single sentence the learner most likely said, using the
// conversation as it stood before this turn — STT ambiguity (homophones,
// mis-heard words) is often only resolvable with that context. It must NOT
// correct grammar or rewrite the sentence into "proper" English: the
// learner's actual mistakes have to reach correct() and the LLM chat context
// unchanged (see session package doc) — this only disambiguates what was
// heard, never rewrites what should have been said.
func (p *Pipeline) synthesizeTranscript(ctx context.Context, client llm.Client, model string, summary string, recent []llm.Message, candidates []string) (string, error) {
	msgs := []llm.Message{
		{Role: llm.RoleSystem, Content: transcriptSynthesisSystemPrompt},
		{Role: llm.RoleUser, Content: renderTranscriptSynthesisInput(summary, recent, candidates)},
	}
	return client.Complete(ctx, model, msgs, false)
}

const transcriptSynthesisSystemPrompt = `You are a transcription-reconciliation assistant for a spoken English
conversation. Independent speech-to-text engines transcribed the SAME short
utterance from a language learner; they may disagree, especially on words
that sound alike. Using the conversation so far for context, decide the
single sentence the learner most likely actually said.
Rules:
- Return ONLY that sentence. No labels, no quotes, no alternatives, no commentary.
- If every candidate already agrees, return it unchanged.
- Preserve the learner's actual words and grammar EXACTLY as transcribed,
  mistakes included — resolve disagreements between the candidates, do not
  correct grammar or rewrite the sentence into "proper" English.`

// writeTranscript appends each message as a "role: content" line to b — the
// shared rendering used everywhere an LLM prompt needs to show msgs as prior
// conversation (transcript synthesis, compaction, translation/correction
// context).
func writeTranscript(b *strings.Builder, msgs []llm.Message) {
	for _, m := range msgs {
		fmt.Fprintf(b, "%s: %s\n", m.Role, m.Content)
	}
}

func renderTranscriptSynthesisInput(summary string, recent []llm.Message, candidates []string) string {
	var b strings.Builder
	if summary != "" {
		b.WriteString("Long-term memory of this learner:\n" + summary + "\n\n")
	}
	if len(recent) > 0 {
		b.WriteString("Conversation so far:\n")
		writeTranscript(&b, recent)
		b.WriteString("\n")
	}
	b.WriteString("Candidate transcriptions of the learner's next line:\n")
	for i, c := range candidates {
		fmt.Fprintf(&b, "%d. %s\n", i+1, c)
	}
	return b.String()
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

// analyze runs one REFINE-track task (grammar correction or compaction)
// across every Analysis candidate concurrently. A single successful
// candidate is returned as-is — nothing to synthesize. Two or more are
// handed to Judge, which picks/merges them into the one final answer; if
// Judge itself fails, the first candidate's answer is used so a flaky judge
// degrades gracefully instead of losing the turn.
func (p *Pipeline) analyze(ctx context.Context, systemPrompt, input string, jsonMode bool) (string, error) {
	if len(p.Analysis) == 0 {
		return "", fmt.Errorf("analyze: no candidates configured")
	}

	type candidateResult struct {
		model string
		text  string
		ok    bool
	}
	msgs := []llm.Message{
		{Role: llm.RoleSystem, Content: systemPrompt},
		{Role: llm.RoleUser, Content: input},
	}

	// Each slot is filled by index, so the results slice stays in
	// Analysis's configured order regardless of completion timing — the
	// judge-error fallback below always means "the first configured
	// candidate", not "whichever happened to finish first".
	slots := fanOutOrdered(len(p.Analysis), func(i int) candidateResult {
		c := p.Analysis[i]
		text, err := c.LLM.Complete(ctx, c.Model, msgs, jsonMode)
		if err != nil {
			log.Printf("analyze: candidate %s: %v", c.Model, err)
			return candidateResult{}
		}
		if text = strings.TrimSpace(text); text == "" {
			return candidateResult{}
		}
		return candidateResult{model: c.Model, text: text, ok: true}
	})

	var results []candidateResult
	for _, r := range slots {
		if r.ok {
			results = append(results, r)
		}
	}

	if len(results) == 0 {
		return "", fmt.Errorf("analyze: every candidate failed")
	}
	if len(results) == 1 {
		return results[0].text, nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Original task:\n%s\n\nOriginal input:\n%s\n\n", systemPrompt, input)
	for _, r := range results {
		fmt.Fprintf(&b, "--- candidate (%s) ---\n%s\n\n", r.model, r.text)
	}
	judgeMsgs := []llm.Message{
		{Role: llm.RoleSystem, Content: judgeSystemPrompt},
		{Role: llm.RoleUser, Content: b.String()},
	}
	final, err := p.Judge.Complete(ctx, p.JudgeModel, judgeMsgs, jsonMode)
	if err != nil {
		log.Printf("analyze: judge: %v; falling back to first candidate", err)
		return results[0].text, nil
	}
	if final = strings.TrimSpace(final); final == "" {
		return results[0].text, nil
	}
	return final, nil
}

const judgeSystemPrompt = `Several candidate models independently performed the same task below.
Synthesize them into the single best final answer, following the ORIGINAL
task's instructions and required output format EXACTLY (e.g. if it asked for
strict JSON, output strict JSON and nothing else). Output ONLY the final
answer — no preamble, no meta-commentary about the candidates or the judging
process.`

// compact folds the oldest verbatim turns into the session's long-term
// summary once the window exceeds MaxHistoryMessages, so long conversations
// stay cheap to send to the LLM and to persist (internal/store). It uses
// context.Background() — bookkeeping on already-committed history, so a
// barge-in on the current turn must not cancel it.
func (p *Pipeline) compact(sess *session.Session) {
	old, curSummary, ok := sess.PeekOldestForCompaction(p.MaxHistoryMessages)
	if !ok {
		return
	}
	newSummary, err := p.analyze(context.Background(), compactionSystemPrompt, renderCompactionInput(curSummary, old), false)
	if err != nil {
		log.Printf("compact: %v", err) // leave history untouched; retried next turn
		return
	}
	sess.ApplyCompaction(strings.TrimSpace(newSummary), len(old))
}

const compactionSystemPrompt = `You maintain compact, persistent memory of an English-learning conversation.
Given the previous summary (may be empty) and a batch of older verbatim turns,
write ONE updated summary that:
- Preserves useful long-term facts about the learner: interests, goals, recurring
  grammar/vocabulary mistakes, proficiency level, and topics already discussed.
- Stays concise (a few sentences to a short paragraph) — this replaces the raw
  turns, it is not a transcript.
- Is written in English.
Return ONLY the updated summary text. No labels, no JSON, no preamble.`

func renderCompactionInput(prevSummary string, old []llm.Message) string {
	var b strings.Builder
	b.WriteString("Previous summary:\n")
	if prevSummary == "" {
		b.WriteString("(none)\n")
	} else {
		b.WriteString(prevSummary + "\n")
	}
	b.WriteString("\nOlder turns to fold in:\n")
	writeTranscript(&b, old)
	return b.String()
}

// correct asks the analysis ensemble for grammar/vocabulary/context feedback
// as strict JSON, synthesized down to one result by analyze(). contextMsg
// (from renderCorrectionContext) is the conversation the sentence was said
// in, folded into the analysis input the same way compaction folds its
// prior-summary context; it is empty on the first turn.
//
// This runs FAST then REFINE, the same shape refine() already uses for
// transcription: AnalyzeCorrectionFast (one quick chat-model call) emits
// first so the learner sees feedback without waiting on the analysis
// ensemble; the ensemble pass below then emits again ONLY if its answer
// actually differs from what the fast pass already showed (see
// correctionsEqual) — the same only-patch-if-it-changed rule refine()
// applies to EvRefined. If the fast pass itself failed, the ensemble's
// result is emitted unconditionally, since there's nothing yet to compare it
// against.
//
// Callers pass context.WithoutCancel(ctx) (see HandleText/refine above), not
// the caller's turn-scoped or connection ctx directly: a barge-in or
// disconnect must not silently drop a grammar-check/translation result that
// was already in flight.
//
// Both onResult and onFailure emit an EvCorrection — the client hangs a
// per-turn pending/spinner state off this event type, so it needs a
// definitive "the check finished" signal regardless of outcome, not just
// when there's a card to show. Without onFailure firing on an analyze()
// error, that spinner used to hang forever (live) or quietly vanish once the
// turn aged out of the frontend's "recently active" window (reloaded) —
// indistinguishable from "already correct", which is exactly the confusion
// this exists to remove. onFailure only fires when the FAST pass also
// failed: once the learner already has a real (fast) result, a slower
// ensemble error is a missed upgrade, not a failure worth reporting.
func (p *Pipeline) correct(ctx context.Context, userID, sessionID string, turn int, text, contextMsg string, emit Emit) {
	emitResult := func(corrected string, issues []protocol.Issue, translation string) {
		// The translation fires independently so a learner still gets a
		// meaning check even on an already-correct sentence.
		emit(protocol.ServerEvent{
			Type: protocol.EvCorrection,
			Turn: turn,
			Correction: &protocol.Correction{
				Original:  text,
				Corrected: corrected,
				Issues:    issues,
			},
		})
		if strings.TrimSpace(translation) != "" {
			emit(protocol.ServerEvent{Type: protocol.EvUserTranslation, Turn: turn, Text: translation})
		}
	}

	fastCorrected, fastIssues, fastTranslation, fastErr := p.AnalyzeCorrectionFast(ctx, text, contextMsg)
	fastOK := fastErr == nil
	if fastOK {
		emitResult(fastCorrected, fastIssues, fastTranslation)
	} else {
		log.Printf("correct: fast pass: %v", fastErr)
	}

	onResult := func(corrected string, issues []protocol.Issue, translation string) {
		if fastOK && correctionsEqual(fastCorrected, fastIssues, fastTranslation, corrected, issues, translation) {
			return // the ensemble agrees with what the learner already sees
		}
		emitResult(corrected, issues, translation)
	}
	onFailure := func() {
		if !fastOK {
			emit(protocol.ServerEvent{Type: protocol.EvCorrection, Turn: turn, Failed: true})
		}
	}
	if p.CorrectHook != nil {
		p.CorrectHook(ctx, userID, sessionID, turn, text, contextMsg, onResult, onFailure)
		return
	}
	corrected, issues, translation, err := p.AnalyzeCorrection(ctx, text, contextMsg)
	if err != nil {
		log.Printf("correct: %v", err)
		onFailure()
		return
	}
	onResult(corrected, issues, translation)
}

// correctionsEqual reports whether two correction results are the same for
// display purposes — used by correct() to decide whether the REFINE
// ensemble pass actually improved on the FAST pass, so a learner is never
// shown a second, identical card.
func correctionsEqual(aCorrected string, aIssues []protocol.Issue, aTranslation string, bCorrected string, bIssues []protocol.Issue, bTranslation string) bool {
	if strings.TrimSpace(aCorrected) != strings.TrimSpace(bCorrected) {
		return false
	}
	if strings.TrimSpace(aTranslation) != strings.TrimSpace(bTranslation) {
		return false
	}
	if len(aIssues) != len(bIssues) {
		return false
	}
	for i := range aIssues {
		if aIssues[i] != bIssues[i] {
			return false
		}
	}
	return true
}

// AnalyzeCorrection runs the grammar/vocabulary/context analysis ensemble
// for one sentence and parses its strict-JSON result. Exported so
// transport's queue-backed CorrectHook implementation reuses the exact same
// call correct() uses directly by default.
func (p *Pipeline) AnalyzeCorrection(ctx context.Context, text, contextMsg string) (corrected string, issues []protocol.Issue, translation string, err error) {
	raw, err := p.analyze(ctx, correctionSystemPrompt(p.FeedbackLang), renderCorrectionInput(contextMsg, text), true)
	if err != nil {
		return "", nil, "", err
	}
	return parseCorrection(raw)
}

// AnalyzeCorrectionFast is AnalyzeCorrection's FAST-track counterpart: one
// call to the chat model (p.LLM/p.ChatModel) instead of the analysis
// ensemble, so correct() can show a result immediately while the slower,
// more accurate ensemble pass keeps running in the background — see
// correct()'s doc comment. Same prompt and JSON shape as AnalyzeCorrection;
// only the model tier differs.
func (p *Pipeline) AnalyzeCorrectionFast(ctx context.Context, text, contextMsg string) (corrected string, issues []protocol.Issue, translation string, err error) {
	msgs := []llm.Message{
		{Role: llm.RoleSystem, Content: correctionSystemPrompt(p.FeedbackLang)},
		{Role: llm.RoleUser, Content: renderCorrectionInput(contextMsg, text)},
	}
	raw, err := p.LLM.Complete(ctx, p.ChatModel, msgs, true)
	if err != nil {
		return "", nil, "", err
	}
	return parseCorrection(raw)
}

// parseCorrection parses AnalyzeCorrection/AnalyzeCorrectionFast's shared
// strict-JSON output shape.
func parseCorrection(raw string) (corrected string, issues []protocol.Issue, translation string, err error) {
	var parsed struct {
		Corrected   string           `json:"corrected"`
		Translation string           `json:"translation"`
		Issues      []protocol.Issue `json:"issues"`
	}
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return "", nil, "", fmt.Errorf("bad json: %w", err)
	}
	return parsed.Corrected, parsed.Issues, parsed.Translation, nil
}

// chatYieldPoll is how often acquireTranslationSlot rechecks chatActive
// while waiting for a gap in chat activity. Translation isn't
// latency-sensitive, so this only needs to be short enough that a
// translation call starts promptly once a chat reply finishes — not tight
// enough to matter for CPU usage.
const chatYieldPoll = 100 * time.Millisecond

// acquireTranslationSlot blocks until at most one translation call is in
// flight (translationSem, capacity 1 — shared by live per-turn translation
// and internal/backfill's worker, since both hold the same *Pipeline) and,
// best-effort, until no chat reply is currently streaming (chatActive):
// translation doesn't need to be real-time, so a NEW translation call yields
// to an in-flight chat reply rather than contending with it for the LLM
// backend. A translation that has already acquired the slot is never
// preempted — only new acquisitions wait on chatActive.
func (p *Pipeline) acquireTranslationSlot(ctx context.Context) error {
	for atomic.LoadInt32(&p.chatActive) > 0 {
		select {
		case <-time.After(chatYieldPoll):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	select {
	case p.translationSemaphore() <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (p *Pipeline) releaseTranslationSlot() { <-p.translationSem }

func (p *Pipeline) translationSemaphore() chan struct{} {
	p.translationSemOnce.Do(func() { p.translationSem = make(chan struct{}, 1) })
	return p.translationSem
}

// translateAssistant asks the analysis ensemble for a plain native-language
// translation of the assistant's full reply, synthesized down to one result
// by analyze() — the same ensemble/Judge machinery correct() uses, just with
// a plain-text (not JSON) prompt since there's nothing else to parse out.
//
// Callers pass context.WithoutCancel(ctx) (see StartConversation/reply
// above), not the turn-scoped or connection ctx directly: this only starts
// once the full reply has already streamed, so in a live conversation it's
// the enrichment most likely to still be running when the learner's next
// utterance (barge-in) or a disconnect cancels ctx — tying this call to that
// context meant it silently lost the race (and the translation) on almost
// every fast back-and-forth exchange. ctx is still threaded through to
// acquireTranslationSlot and analyze() (rather than dropping it) so a caller
// that legitimately wants early cancellation — like internal/backfill's
// long-lived worker ctx via TranslateWithContext below — still gets it.
func (p *Pipeline) translateAssistant(ctx context.Context, userID, sessionID string, turn int, text string, emit Emit) {
	// FAST pass: one quick chat-model call so the learner sees a translation
	// immediately; REFINE (below) re-checks it with the analysis ensemble and
	// silently patches the result only if it disagrees — same shape as
	// correct()'s two-stage flow.
	fastTranslation, fastErr := p.AnalyzeTranslationFast(ctx, text)
	fastOK := fastErr == nil
	if fastOK {
		emit(protocol.ServerEvent{Type: protocol.EvAssistantTranslation, Turn: turn, Text: fastTranslation})
	} else {
		log.Printf("translateAssistant: fast pass: %v", fastErr)
	}

	onResult := func(translation string) {
		if fastOK && strings.TrimSpace(fastTranslation) == strings.TrimSpace(translation) {
			return // the ensemble agrees with what the learner already sees
		}
		// analyze() never succeeds with a blank result (a candidate's own
		// empty output is filtered out before it can win), so this event
		// always carries real text — the client's pending/spinner state
		// (see App.tsx) treats this event's arrival as the "translation
		// finished" signal.
		emit(protocol.ServerEvent{Type: protocol.EvAssistantTranslation, Turn: turn, Text: translation})
	}
	if p.TranslateHook != nil {
		p.TranslateHook(ctx, userID, sessionID, turn, text, onResult)
		return
	}
	translation, err := p.AnalyzeTranslation(ctx, text)
	if err != nil {
		log.Printf("translateAssistant: %v", err)
		return
	}
	onResult(translation)
}

// AnalyzeTranslation translates one assistant reply's full text into the
// learner's native language, yielding to any in-flight chat reply first
// (see acquireTranslationSlot). Exported so transport's queue-backed
// TranslateHook implementation reuses the exact same call
// translateAssistant() uses directly by default.
func (p *Pipeline) AnalyzeTranslation(ctx context.Context, text string) (string, error) {
	if err := p.acquireTranslationSlot(ctx); err != nil {
		return "", err
	}
	defer p.releaseTranslationSlot()
	raw, err := p.analyze(ctx, translationSystemPrompt(p.FeedbackLang), text, false)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(raw), nil
}

// AnalyzeTranslationFast is AnalyzeTranslation's FAST-track counterpart: one
// call to the chat model (p.LLM/p.ChatModel) instead of the analysis
// ensemble, mirroring AnalyzeCorrectionFast — see translateAssistant()'s
// two-stage flow. Unlike AnalyzeTranslation, it does not go through
// acquireTranslationSlot: it isn't contending with the Analysis ensemble's
// backend, and the whole point is to answer before that slot would even
// matter. A blank (whitespace-only) result is treated as an error, matching
// analyze()'s own "no blank winners" contract.
func (p *Pipeline) AnalyzeTranslationFast(ctx context.Context, text string) (string, error) {
	msgs := []llm.Message{
		{Role: llm.RoleSystem, Content: translationSystemPrompt(p.FeedbackLang)},
		{Role: llm.RoleUser, Content: text},
	}
	raw, err := p.LLM.Complete(ctx, p.ChatModel, msgs, false)
	if err != nil {
		return "", err
	}
	if trimmed := strings.TrimSpace(raw); trimmed != "" {
		return trimmed, nil
	}
	return "", fmt.Errorf("translateFast: empty result")
}

// TranslateWithContext translates text into the learner's native language
// (FeedbackLang), using priorTurns — verbatim, in order — as context, the
// same "conversation so far" shape correct() feeds the grammar pass via
// renderCorrectionContext, so a re-translation reads the same as if it had
// been generated live rather than in isolation. Unlike translateAssistant
// (which translates a just-produced reply with no ambiguity to resolve),
// this exists for internal/backfill: filling in a translation for a turn
// that never got one the first time, possibly long after the turns around
// it were said.
func (p *Pipeline) TranslateWithContext(ctx context.Context, priorTurns []llm.Message, text string) (string, error) {
	if err := p.acquireTranslationSlot(ctx); err != nil {
		return "", err
	}
	defer p.releaseTranslationSlot()
	raw, err := p.analyze(ctx, translationSystemPrompt(p.FeedbackLang), renderTranslationInput(renderTranslationContext(priorTurns), text), false)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(raw), nil
}

// CorrectWithContext runs the grammar-correction analysis ensemble for one
// turn using priorTurns — verbatim, in order — as context, the same
// "conversation so far" shape correct() feeds via renderCorrectionContext, so
// a backfilled correction reads the same as if it had been generated live.
// Unlike correct() (which always has the session's long-term summary
// available), this exists for internal/backfill, which only has the raw turn
// history — mirrors TranslateWithContext's reasoning for the identical gap on
// the translation side. Does not go through acquireTranslationSlot: that only
// guards the shared translation LLM call, which this doesn't make (the
// "translation" it returns is AnalyzeCorrection's own translation-of-the-
// original-sentence field, produced by the SAME analysis call as the
// correction, not a second one).
func (p *Pipeline) CorrectWithContext(ctx context.Context, priorTurns []llm.Message, text string) (corrected string, issues []protocol.Issue, translation string, err error) {
	return p.AnalyzeCorrection(ctx, text, renderCorrectionContext("", priorTurns))
}

// StudyIssue pairs one flagged grammar/vocabulary/phrasing/context issue
// with the learner's original sentence it came from — the raw material
// GenerateStudySummary synthesizes into a session-wide "what to study next"
// recommendation. Callers assemble this from a session's persisted
// corrections (see httpserver.sessionStudySummaryHandler); Pipeline itself
// tracks nothing session-wide.
type StudyIssue struct {
	Text  string
	Issue protocol.Issue
}

// GenerateStudySummary synthesizes every grammar/vocabulary/phrasing/context
// issue flagged across a session into one encouraging wrap-up: the recurring
// PATTERNS a learner should focus on studying next, rather than a re-listing
// of each individual correction (already visible via the frontend's
// FeedbackSummary/GrammarControl). Written in English — the language being
// learned — sentence by sentence, each paired with a native-language
// translation, the same teach-in-English-then-translate shape
// correctionSystemPrompt uses for "explanation"/"explanationTranslation".
// Meant to be called once, when the learner explicitly ends a conversation —
// not on every reload — so unlike GenerateTitle (a decorative, cost-sensitive
// single call) this affords the full Analysis ensemble+Judge, the same
// learning-facing quality bar as correct()/compact() use. Callers should
// skip this call entirely when issues is empty (see
// sessionStudySummaryHandler) rather than spend an LLM call being told
// there's nothing to report.
func (p *Pipeline) GenerateStudySummary(ctx context.Context, issues []StudyIssue) ([]protocol.StudySummarySentence, error) {
	raw, err := p.analyze(ctx, studySummarySystemPrompt(p.FeedbackLang), renderStudySummaryInput(issues), true)
	if err != nil {
		return nil, err
	}
	var parsed struct {
		Sentences []protocol.StudySummarySentence `json:"sentences"`
	}
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return nil, fmt.Errorf("study summary: bad json: %w", err)
	}
	return parsed.Sentences, nil
}

// studySummarySystemPrompt builds GenerateStudySummary's prompt, reusing the
// same native-language config as correctionSystemPrompt/translationSystemPrompt.
// The wrap-up itself is authored in English, sentence by sentence, each
// immediately paired with its native-language translation — same reasoning
// as correctionSystemPrompt's explanation/explanationTranslation split: the
// learner reads real English first, with the translation there only to make
// sure the meaning actually lands.
func studySummarySystemPrompt(lang string) string {
	native := languageName(lang)
	return fmt.Sprintf(`You are an encouraging English-conversation coach wrapping up one practice
session with a %[1]s-speaking learner. You are given every grammar/vocabulary/
phrasing/context issue flagged during the session, each paired with the
learner's original sentence.
Write a short wrap-up that helps the learner study on their own afterward:
- Group individual issues into the recurring PATTERNS behind them (e.g.
  repeated third-person -s omission, article misuse, a specific mistranslated
  collocation) instead of re-listing each correction one by one.
- Call out 2-4 concrete things to focus on next, roughly ordered by how often
  they came up.
- End on an encouraging note.
Write the wrap-up in English, as a sequence of short, complete sentences (a
few short paragraphs or a short list is fine, but split it into individual
sentences rather than one long block) — natural, encouraging coaching
language, not a dry report.
Return STRICT JSON only, no prose, in exactly this shape:
{"sentences":[{"english":"<one sentence of the wrap-up, in English>","translation":"<natural %[1]s translation of that same sentence>"}]}
Rules:
- "english" MUST stay in English.
- "translation" MUST be a translation of "english", not new or different content.
- Every sentence of the wrap-up must appear as its own array entry, in reading order.`, native)
}

func renderStudySummaryInput(issues []StudyIssue) string {
	var b strings.Builder
	b.WriteString("Issues flagged during this session:\n")
	for _, si := range issues {
		fmt.Fprintf(&b, "- sentence: %q | type: %s | %q -> %q | %s\n",
			si.Text, si.Issue.Type, si.Issue.Span, si.Issue.Suggestion, si.Issue.Explanation)
	}
	return b.String()
}

// GenerateStudyQuiz synthesizes a short fill-in-the-blank practice quiz from
// every grammar/vocabulary/phrasing/context issue flagged across a session —
// a way to test whether a learner actually absorbed GenerateStudySummary's
// prose wrap-up, not just read it. Each question blanks out the word/phrase
// one recurring pattern is about, in a fresh example sentence rather than
// the learner's own original wording, so answering it requires applying the
// rule instead of recalling a specific sentence. Same Analysis ensemble+Judge
// quality bar as GenerateStudySummary — a wrong "correct" answer here would
// actively mislead a learner practicing on their own. Generated on demand,
// only when a learner opens the quiz (see httpserver.sessionQuizHandler),
// not automatically alongside the wrap-up — callers should skip this call
// entirely when issues is empty, same reasoning as GenerateStudySummary.
func (p *Pipeline) GenerateStudyQuiz(ctx context.Context, issues []StudyIssue) ([]protocol.QuizQuestion, error) {
	raw, err := p.analyze(ctx, quizSystemPrompt(p.FeedbackLang), renderStudySummaryInput(issues), true)
	if err != nil {
		return nil, err
	}
	var parsed struct {
		Questions []protocol.QuizQuestion `json:"questions"`
	}
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return nil, fmt.Errorf("study quiz: bad json: %w", err)
	}
	return parsed.Questions, nil
}

// quizSystemPrompt builds GenerateStudyQuiz's prompt, reusing the same
// native-language config and issue-listing input (renderStudySummaryInput)
// as studySummarySystemPrompt.
func quizSystemPrompt(lang string) string {
	native := languageName(lang)
	return fmt.Sprintf(`You are an English-conversation coach building a short practice quiz from
every grammar/vocabulary/phrasing/context issue flagged during one learner's
practice session, each paired with the learner's original sentence.
Write 3-5 fill-in-the-blank questions that test the recurring PATTERNS behind
those issues (e.g. repeated third-person -s omission, article misuse, a
specific mistranslated collocation) — write a FRESH example sentence for
each pattern rather than reusing the learner's original sentence verbatim,
so answering requires applying the rule, not recalling a specific sentence.
For each question:
- "prompt" is a natural English sentence with exactly one blank, written as
  "___", where the tested word or phrase belongs.
- "answer" is the exact word or phrase that correctly fills that blank.
- "translation" is a natural %[1]s translation of the FULL sentence with the
  blank correctly filled in, so the learner can check they understood the
  meaning even if they get the blank wrong.
- "explanation" briefly says, in English, why that's the answer.
- "explanationTranslation" is a natural %[1]s translation of "explanation".
Return STRICT JSON only, no prose, in exactly this shape:
{"questions":[{"prompt":"<sentence with one ___ blank>","answer":"<the word/phrase that fills it>","translation":"<%[1]s translation of the full correct sentence>","explanation":"<why, in English>","explanationTranslation":"<%[1]s translation of explanation>"}]}
Rules:
- "prompt", "answer", and "explanation" MUST stay in English.
- "translation" and "explanationTranslation" MUST be written in %[1]s.
- Every "prompt" must contain exactly one "___".
- Prefer variety: don't test the same single pattern more than twice.`, native)
}

// UpdateLearnerProfile folds one just-ended session's study wrap-up (see
// GenerateStudySummary, written in the learner's native language) into
// userID's persistent, cross-session profile (store.Store.GetLearnerProfile/
// SaveLearnerProfile) — the same "roll the old summary and new material into
// one updated summary" shape compact() uses for a single session's own
// long-term memory, except this one spans every conversation the learner has
// ever ended, not just turns within one room. Called once, from
// httpserver.sessionEndHandler, when the learner confirms "end this
// conversation" — never on every reload.
func (p *Pipeline) UpdateLearnerProfile(ctx context.Context, prevProfile, sessionSummary string) (string, error) {
	raw, err := p.analyze(ctx, learnerProfileSystemPrompt, renderLearnerProfileInput(prevProfile, sessionSummary), false)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(raw), nil
}

// learnerProfileSystemPrompt is written in English and asks for an
// English-only result: unlike GenerateStudySummary's output (shown directly
// to the learner, so it's in their native language), this profile is only
// ever machine-consumed — folded into BuildSystemPrompt's system prompt for
// a brand-new session — the same reasoning compactionSystemPrompt uses.
const learnerProfileSystemPrompt = `You maintain a persistent, cross-conversation profile of an
English-learning student, used as background context in future practice sessions.
Given the previous profile (may be empty) and a new wrap-up note from a conversation
that was just ended (written in the learner's native language), write ONE updated
profile that:
- Preserves and updates useful long-term facts: interests, goals, proficiency level,
  and recurring grammar/vocabulary/phrasing patterns the learner should keep working on.
- Stays concise (a few sentences to a short paragraph) — it is background context for
  an LLM, not a report to show the learner.
- Is written in English, regardless of what language the new note is in.
Return ONLY the updated profile text. No labels, no JSON, no preamble.`

func renderLearnerProfileInput(prevProfile, sessionSummary string) string {
	var b strings.Builder
	b.WriteString("Previous profile:\n")
	if prevProfile == "" {
		b.WriteString("(none)\n")
	} else {
		b.WriteString(prevProfile + "\n")
	}
	b.WriteString("\nNew session wrap-up to fold in:\n")
	b.WriteString(sessionSummary)
	return b.String()
}

// RunTitle asks for a chat-room title from transcript and saves it, either
// via TitleHook (durable, queue-backed — see transport.NewTitleHook) or by
// calling GenerateTitle and save directly in-process — same
// hook-if-set/direct-otherwise shape as runReply/correct/translateAssistant.
// Called by internal/transport on turn 1 and every TitleRegenerateEveryNTurns
// turns after that (see Handler.generateTitle), not from Pipeline's own
// turn-handling, since triggering it depends on transport's turn-counting
// convention, not anything Pipeline itself tracks. save persists the
// generated title (there's no ServerEvent for it — the frontend just
// re-fetches the room list — so unlike the other hooks' onResult, this
// exists purely for the direct/no-hook fallback; TitleHook is responsible
// for persisting on its own path, see transport.TitleJobHandler).
func (p *Pipeline) RunTitle(ctx context.Context, userID, sessionID string, turn int, transcript []llm.Message, save func(title string) error) {
	if p.TitleHook != nil {
		p.TitleHook(ctx, userID, sessionID, turn, transcript)
		return
	}
	title, err := p.GenerateTitle(ctx, transcript)
	if err != nil {
		log.Printf("title: generate %s/%s: %v", userID, sessionID, err)
		return
	}
	if title = strings.TrimSpace(title); title == "" {
		return
	}
	if err := save(title); err != nil {
		log.Printf("title: save %s/%s: %v", userID, sessionID, err)
	}
}

// GenerateTitle asks the LLM for a short, descriptive chat-room title from
// the conversation so far — called by RunTitle above (directly, or from
// within transport.TitleJobHandler when TitleHook is set) — replacing the
// raw truncated-first-message placeholder store.MySQLStore.SaveTurn sets on
// turn 1, and again every TitleRegenerateEveryNTurns turns after that, so
// the title keeps tracking the conversation's actual topic rather than
// staying pinned to turn 1's. A single fast call (p.LLM/p.ChatModel), not
// the Analysis ensemble+Judge like analyze() uses: a room label is
// decorative, not something a learner's grammar feedback depends on, so it
// isn't worth doubling the LLM cost of every title (re)generation.
func (p *Pipeline) GenerateTitle(ctx context.Context, transcript []llm.Message) (string, error) {
	msgs := []llm.Message{
		{Role: llm.RoleSystem, Content: titleSystemPrompt},
		{Role: llm.RoleUser, Content: renderTitleInput(transcript)},
	}
	title, err := p.LLM.Complete(ctx, p.ChatModel, msgs, false)
	if err != nil {
		return "", err
	}
	// Small models like to wrap a short title in quotes despite being told
	// not to; strip them so the stored title doesn't carry literal "" marks.
	return strings.Trim(strings.TrimSpace(title), `"“”`), nil
}

const titleSystemPrompt = `Give a short, descriptive title for a chat conversation, based on
the conversation so far (it may be just the opening exchange, or many turns
in). The title summarizes the current TOPIC being discussed, not the
learner's exact words.
Rules:
- 2-6 words.
- Plain text: no quotes, no trailing punctuation, no labels like "Title:".
- Write it in the same language the learner used.
- Return ONLY the title, nothing else.`

func renderTitleInput(transcript []llm.Message) string {
	var b strings.Builder
	writeTranscript(&b, transcript)
	return b.String()
}

// SuggestWords asks the chat model for candidate English words/phrases
// matching a learner's native-language description of a word they can't
// recall mid-conversation (e.g. "화가 나서 참을 수 없는 느낌" -> "furious"/"livid").
// A fresh two-message call, same "never touches the session's own
// transcript" shape as GenerateTitle/analyze()-based tasks. Unlike the
// Analysis-ensemble tasks (GenerateStudySummary, correct(), ...), this uses a
// single fast call (p.LLM/p.ChatModel) — the same tier as GenerateTitle: it
// backs a live side panel a learner consults mid-typing, so latency matters
// more than the higher quality bar correct()/GenerateStudySummary need, and
// a mediocre suggestion just gets re-asked.
func (p *Pipeline) SuggestWords(ctx context.Context, description string) ([]protocol.WordSuggestion, error) {
	msgs := []llm.Message{
		{Role: llm.RoleSystem, Content: wordSuggestionSystemPrompt(p.FeedbackLang)},
		{Role: llm.RoleUser, Content: description},
	}
	raw, err := p.LLM.Complete(ctx, p.ChatModel, msgs, true)
	if err != nil {
		return nil, err
	}
	var parsed struct {
		Suggestions []protocol.WordSuggestion `json:"suggestions"`
	}
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return nil, fmt.Errorf("word suggestion: bad json: %w", err)
	}
	return parsed.Suggestions, nil
}

// wordSuggestionSystemPrompt builds SuggestWords' prompt, reusing the same
// native-language config as correctionSystemPrompt/studySummarySystemPrompt.
func wordSuggestionSystemPrompt(lang string) string {
	native := languageName(lang)
	return fmt.Sprintf(`You help a %[1]s-speaking English learner who is mid-conversation and can't
recall an English word, by suggesting candidates from a %[1]s description of
what they mean.
Given a short %[1]s phrase describing a word or concept, suggest 3-5 English
word/phrase candidates that best match it, most likely match first.
Return STRICT JSON only, no prose, in exactly this shape:
{"suggestions":[{"word":"<English word or short phrase>","meaning":"<brief %[1]s gloss>","example":"<one example English sentence using it>"}]}
Rules:
- "word" MUST stay in English.
- "meaning" MUST be written in %[1]s.
- "example" MUST be a natural English sentence that uses "word".
- If the description is too vague to suggest anything meaningful, return an empty "suggestions" array rather than guessing wildly.`, native)
}

// renderTranslationContext formats prior turns as context for
// TranslateWithContext, mirroring renderCorrectionContext's shape but
// labeled for translation rather than correction so the prompt never
// suggests grammar-fixing is in scope. Returns "" when there's nothing to
// give (e.g. translating a session's very first turn).
func renderTranslationContext(priorTurns []llm.Message) string {
	if len(priorTurns) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("Conversation so far, for context only:\n")
	writeTranscript(&b, priorTurns)
	return b.String()
}

// renderTranslationInput combines the context block with the text under
// translation, the same delimited-data pattern renderCorrectionInput uses so
// a replayed turn in the context can't act as an instruction. With no
// context it's just the bare text, matching translateAssistant's input shape.
func renderTranslationInput(contextMsg, text string) string {
	if contextMsg == "" {
		return text
	}
	return contextMsg + "\nText to translate:\n" + text
}

// renderCorrectionContext formats the pre-turn long-term summary and recent
// turns as a context block for the correction pass, or "" when there's
// nothing to give (the learner's first turn). renderCorrectionInput folds it
// in front of the sentence under correction — kept as delimited data in the
// analysis input (like renderCompactionInput), not a privileged system
// message, so a replayed learner utterance can't act as an instruction.
func renderCorrectionContext(summary string, priorTurns []llm.Message) string {
	if summary == "" && len(priorTurns) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("Conversation so far, for judging fit only — do not correct this part:\n")
	if summary != "" {
		b.WriteString("Long-term memory of this learner: " + summary + "\n")
	}
	writeTranscript(&b, priorTurns)
	return b.String()
}

// renderCorrectionInput combines the context block with the sentence under
// correction into the single analysis input. With no context it's just the
// bare sentence, so the first turn behaves exactly as before this existed.
func renderCorrectionInput(contextMsg, text string) string {
	if contextMsg == "" {
		return text
	}
	return contextMsg + "\nSentence to correct:\n" + text
}

// correctionSystemPrompt builds the grammar-coach prompt. The corrected
// sentence, span, suggestion, and explanation stay in English (the language
// being learned); "translation" and "explanationTranslation" are the only
// fields written in the learner's native language, so the reasoning behind
// each fix is taught in English first and then made easy to understand via
// its translation, rather than being authored directly in the native language.
//
// The "Work in this order" block below exists so each issue is derived from
// an actual before/after comparison instead of being judged in one holistic
// guess: a model asked for "corrected" and "issues" in the same breath tends
// to rationalize issues that don't match what it actually changed, or miss
// ones it did change but forgot to mention. Forcing correct-then-diff, plus
// the verbatim span/suggestion rule below, ties every explanation back to a
// specific textual change instead of a generic restated rule.
func correctionSystemPrompt(lang string) string {
	native := languageName(lang)
	return fmt.Sprintf(`You are an English writing coach for a %[1]s-speaking learner.
The input is one spoken English sentence, possibly with speech-to-text noise.
It may be preceded by a "Conversation so far" block for context; if so, correct
only the sentence after "Sentence to correct:", and use the context solely to
judge whether that sentence fits (e.g. pronoun/tense agreement with earlier
turns, actually answering what was asked) — never correct the context itself.

Work in this order, silently — output only the final JSON, never your
intermediate steps:
1. First rewrite the sentence into natural, idiomatic English. This becomes
   "corrected". Fix everything a native speaker would actually change, not
   just outright grammar errors (see "Already correct" below).
2. Then compare "corrected" against the original sentence, word by word, and
   list every difference as an issue. Never report an issue that isn't a
   difference you actually made in step 1, and never leave a difference from
   step 1 unreported.
3. For each issue, write "explanation" by naming that specific before →
   after change and why it was needed (e.g. "go" → "goes": the subject "he"
   is third person singular) — not a generic rule disconnected from this
   sentence.
Return STRICT JSON only, no prose, in exactly this shape:
{"corrected":"<the sentence rewritten in correct, natural English>",
 "translation":"<natural, colloquial %[1]s translation of the ORIGINAL sentence under correction, so the learner can check it against what they meant to say>",
 "issues":[{"type":"grammar|vocabulary|phrasing|context","span":"<the exact original words this issue changes>","suggestion":"<the exact replacement words, as they appear in corrected>","explanation":"<the specific before -> after change and why, written in English, short and kind>","explanationTranslation":"<natural %[1]s translation of explanation, so the reasoning is easy to understand>"}]}
Rules:
- "corrected", "span", "suggestion", and "explanation" MUST stay in English.
- "translation" and "explanationTranslation" MUST be written in %[1]s.
- "translation" MUST translate the ORIGINAL sentence, not the corrected one.
- "explanationTranslation" MUST be a translation of "explanation", not a new or different explanation.
- "span" MUST be verbatim text from the original sentence, and "suggestion" MUST be verbatim text from "corrected" — if you can't point to both, it isn't a real issue. The one exception is the "context" type below, where the fix isn't a simple word swap.
- Use "context" as the issue type only when the sentence is fine in isolation but doesn't fit the conversation (wrong pronoun/tense given earlier turns, doesn't answer what was actually asked, etc.) — "span"/"suggestion" may describe the mismatch in that case instead of quoting exact words.
- "Already correct" means natural, idiomatic English, not merely grammatically parseable. A sentence with no outright grammar error can still need a "phrasing" or "vocabulary" issue if a native speaker would not say it that way — e.g. an unnatural collocation ("using AI in working" instead of "using AI in our work"), a redundant or missing article ("the AI" for a general concept instead of "AI"), or a stiff/awkward word choice. Flag these too.
- If the sentence is already correct, "corrected" equals the original and "issues" is empty — still fill in "translation".`, native)
}

// translationSystemPrompt builds a plain-text translation prompt, reusing
// the same native-language config as correctionSystemPrompt so both stay in
// sync if FeedbackLang changes. Used both for translateAssistant's plain
// reply text and TranslateWithContext's context-prefixed input.
func translationSystemPrompt(lang string) string {
	native := languageName(lang)
	return fmt.Sprintf(`Translate the given English text into natural, colloquial %[1]s for a language learner.
The input may be preceded by a "Conversation so far" block for context; if so,
translate only the text after "Text to translate:", using the context solely
to disambiguate meaning (pronouns, ellipsis, etc.) — never translate the
context itself.
Return ONLY the translation — no prose, no quotes, no labels, no explanation.`, native)
}

// languageName maps a short language code to an English name the LLM
// understands. Unknown codes fall back to the code itself.
func languageName(code string) string {
	switch strings.ToLower(strings.TrimSpace(code)) {
	case "ko", "ko-kr":
		return "Korean"
	case "en", "en-us":
		return "English"
	case "ja", "ja-jp":
		return "Japanese"
	case "zh", "zh-cn":
		return "Chinese"
	case "es":
		return "Spanish"
	case "", "auto":
		return "Korean"
	default:
		return code
	}
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
