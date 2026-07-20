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

const DefaultSystemPrompt = `You are Buddy, a warm, encouraging English conversation partner.
Keep the conversation flowing naturally: reply in 1-3 short spoken-style sentences,
ask a follow-up question, and match the learner's level. Do NOT correct grammar
inline — corrections are handled separately. Never mention that you are an AI.`

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
}

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

// HandleUtterance runs one turn from raw audio.
func (p *Pipeline) HandleUtterance(ctx context.Context, sess *session.Session, pcm []byte, emit Emit) {
	turn := sess.NextTurn()
	// Context as of BEFORE this utterance — used for both the FAST and
	// REFINE reconciliation passes below so they answer the exact same
	// question (same inputs, different model/care level), and so the
	// just-appended (possibly wrong) guess never contaminates its own
	// disambiguation context.
	summary, recent := sess.Export()

	// --- FAST STT ensemble -------------------------------------------------
	userText, candidates, err := p.transcribe(ctx, summary, recent, pcm)
	if err != nil {
		emit(protocol.ServerEvent{Type: protocol.EvError, Turn: turn, Text: "stt: " + err.Error()})
		return
	}
	if userText == "" {
		return
	}
	emit(protocol.ServerEvent{Type: protocol.EvFinal, Turn: turn, Text: userText, Source: protocol.SourceVoice})
	sess.AppendUser(userText)

	// --- REFINE track (background) --------------------------------------
	go p.refine(ctx, sess, turn, summary, recent, candidates, userText, emit)

	// --- FAST reply ------------------------------------------------------
	p.reply(ctx, sess, turn, emit)
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
	slots := make([]slot, len(p.STT))
	var wg sync.WaitGroup
	for i, rec := range p.STT {
		wg.Add(1)
		go func(i int, rec stt.Recognizer) {
			defer wg.Done()
			res, err := rec.Transcribe(ctx, pcm)
			if err != nil {
				log.Printf("transcribe: %s: %v", rec.Name(), err)
				slots[i] = slot{err: err}
				return
			}
			slots[i] = slot{text: strings.TrimSpace(res.Text)}
		}(i, rec)
	}
	wg.Wait()

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

// HandleText runs one turn from typed input (skips STT).
func (p *Pipeline) HandleText(ctx context.Context, sess *session.Session, text string, emit Emit) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	turn := sess.NextTurn()
	emit(protocol.ServerEvent{Type: protocol.EvFinal, Turn: turn, Text: text, Source: protocol.SourceText})
	// Capture the conversation as of BEFORE this turn, so the correction
	// pass can judge whether the sentence fits without the sentence itself
	// contaminating its own context (mirrors HandleUtterance).
	contextMsg := renderCorrectionContext(sess.Export())
	sess.AppendUser(text)
	// context.WithoutCancel: correction must finish and persist even if the
	// learner closes the tab or the socket drops right after sending — same
	// reasoning as backupAudio/compact using a context independent of ctx,
	// just without a fixed deadline since the LLM client already caps itself
	// (see llm.NewOpenAI's http.Client timeout).
	go p.correct(context.WithoutCancel(ctx), turn, text, contextMsg, emit) // correction only; no slow STT needed
	p.reply(ctx, sess, turn, emit)
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
// turn 0 into the transcript like any other turn, but store.MySQLStore.SaveTurn
// only ever creates the session row once the learner's own turn 1 lands — so
// an abandoned "new chat" that's never replied to still leaves no visible
// trace (no row in buddy_sessions, so ListSessions/SessionDetail act as if it
// never happened), while a room the learner does reply to keeps the greeting
// in its transcript instead of losing it on reload. The greeting also lives
// in the in-memory session history, so the LLM sees it as context, and it
// rides along in Profile.Recent once a real reply persists.
func (p *Pipeline) StartConversation(ctx context.Context, sess *session.Session, emit Emit) {
	const openingTurn = 0
	msgs := append(sess.Snapshot(), llm.Message{Role: llm.RoleSystem, Content: openingSystemPrompt})
	atomic.AddInt32(&p.chatActive, 1)
	full, err := p.LLM.ChatStream(ctx, p.ChatModel, msgs, func(tok string) {
		emit(protocol.ServerEvent{Type: protocol.EvAssistantDelta, Turn: openingTurn, Text: tok})
	})
	atomic.AddInt32(&p.chatActive, -1)
	// The connection closed while this was still streaming: drop it rather
	// than append a greeting nobody will ever see.
	if ctx.Err() != nil {
		return
	}
	if err != nil {
		log.Printf("chat: opening: %v", err)
		if strings.TrimSpace(full) == "" {
			full = openingFallback
			emit(protocol.ServerEvent{Type: protocol.EvAssistantDelta, Turn: openingTurn, Text: full})
		}
	}
	emit(protocol.ServerEvent{Type: protocol.EvAssistantDone, Turn: openingTurn, Text: full})
	sess.AppendAssistant(full)
	if strings.TrimSpace(full) != "" {
		// context.WithoutCancel: see reply()'s matching translateAssistant call
		// for why this must survive the connection closing.
		go p.translateAssistant(context.WithoutCancel(ctx), openingTurn, full, emit)
	}
}

const openingSystemPrompt = `Start the conversation: the learner has not said anything yet. Greet them
warmly in 1-2 short spoken-style sentences and ask one easy opening question
to get them talking (e.g. their day, an interest, or what they'd like to
practice). Do not mention that you were told to do this.`

const openingFallback = "Hey there! Glad you're here — what would you like to talk about today?"

// reply streams the assistant response and records it in the session.
func (p *Pipeline) reply(ctx context.Context, sess *session.Session, turn int, emit Emit) {
	msgs := sess.Snapshot()
	atomic.AddInt32(&p.chatActive, 1)
	full, err := p.LLM.ChatStream(ctx, p.ChatModel, msgs, func(tok string) {
		emit(protocol.ServerEvent{Type: protocol.EvAssistantDelta, Turn: turn, Text: tok})
	})
	atomic.AddInt32(&p.chatActive, -1)
	// Barged in (user spoke again): drop this turn's tail, keep history clean.
	if ctx.Err() != nil {
		return
	}
	if err != nil {
		log.Printf("chat: %v", err)
		if strings.TrimSpace(full) == "" {
			full = fallbackReply(msgs)
			emit(protocol.ServerEvent{Type: protocol.EvAssistantDelta, Turn: turn, Text: full})
		}
	}
	emit(protocol.ServerEvent{Type: protocol.EvAssistantDone, Turn: turn, Text: full})
	sess.AppendAssistant(full)
	if strings.TrimSpace(full) != "" {
		// context.WithoutCancel: the reply already streamed to the learner, so
		// its translation is a self-contained piece of work with nothing left
		// to race — a barge-in or disconnect right as it starts must not lose
		// it, same reasoning as backupAudio/persistEvent detaching from ctx.
		go p.translateAssistant(context.WithoutCancel(ctx), turn, full, emit)
	}
	go p.compact(sess) // background: fold old turns into the long-term summary
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

	// Each goroutine owns a fixed slot by index, so the results slice stays
	// in Analysis's configured order regardless of completion timing — the
	// judge-error fallback below always means "the first configured
	// candidate", not "whichever happened to finish first".
	slots := make([]candidateResult, len(p.Analysis))
	var wg sync.WaitGroup
	for i, c := range p.Analysis {
		wg.Add(1)
		go func(i int, c Candidate) {
			defer wg.Done()
			text, err := c.LLM.Complete(ctx, c.Model, msgs, jsonMode)
			if err != nil {
				log.Printf("analyze: candidate %s: %v", c.Model, err)
				return
			}
			if text = strings.TrimSpace(text); text == "" {
				return
			}
			slots[i] = candidateResult{model: c.Model, text: text, ok: true}
		}(i, c)
	}
	wg.Wait()

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

// refine hands transcribe()'s STT candidates to Judge for a second, more
// careful reconciliation pass — a stronger model, the exact same pre-turn
// context — instead of re-running STT. This is the background pass that
// keeps reducing transcription errors in the conversation: if Judge lands on
// something different from the FAST track's quick pick, the session's user
// turn is upgraded and refined_transcript is emitted; if it agrees (or
// fails), nothing changes. Runs even with a single STT candidate — Judge can
// still catch a mishearing FAST's quick pass didn't, using context alone.
// A nil Judge (unconfigured) just skips reconciliation — correct() still runs.
func (p *Pipeline) refine(ctx context.Context, sess *session.Session, turn int, summary string, recent []llm.Message, candidates []string, fastText string, emit Emit) {
	refined := fastText
	if p.Judge != nil {
		text, err := p.synthesizeTranscript(ctx, p.Judge, p.JudgeModel, summary, recent, candidates)
		if err != nil {
			log.Printf("refine: %v", err)
		} else if text = strings.TrimSpace(text); text != "" {
			refined = text
		}
	}
	if refined != fastText {
		emit(protocol.ServerEvent{Type: protocol.EvRefined, Turn: turn, Text: refined, Source: protocol.SourceVoice})
		sess.ReplaceLastUser(refined) // keep future context accurate
	}
	// summary/recent are the pre-turn context captured in HandleUtterance
	// before this utterance was appended — so it never includes the sentence
	// under correction, regardless of how the concurrent reply() interleaves.
	//
	// context.WithoutCancel here (unlike the Judge call above, which stays on
	// ctx since sess.ReplaceLastUser must respect turn ordering/barge-in):
	// correct() only emits+persists a result for this fixed turn number and
	// never touches sess, so there's no ordering hazard in letting it outlive
	// a barge-in or disconnect — same reasoning as HandleText's correct call.
	p.correct(context.WithoutCancel(ctx), turn, refined, renderCorrectionContext(summary, recent), emit)
}

// correct asks the analysis ensemble for grammar/vocabulary/context feedback
// as strict JSON, synthesized down to one result by analyze(). contextMsg
// (from renderCorrectionContext) is the conversation the sentence was said
// in, folded into the analysis input the same way compaction folds its
// prior-summary context; it is empty on the first turn.
func (p *Pipeline) correct(ctx context.Context, turn int, text, contextMsg string, emit Emit) {
	raw, err := p.analyze(ctx, correctionSystemPrompt(p.FeedbackLang), renderCorrectionInput(contextMsg, text), true)
	if err != nil {
		log.Printf("correct: %v", err)
		return
	}
	var parsed struct {
		Corrected   string           `json:"corrected"`
		Translation string           `json:"translation"`
		Issues      []protocol.Issue `json:"issues"`
	}
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		log.Printf("correct: bad json: %v", err)
		return
	}
	// Always emit, even when there's nothing to teach (Corrected == text,
	// empty Issues) — the client hangs a per-turn pending/spinner state off
	// this event, so it needs a definitive "the check finished" signal
	// regardless of outcome, not just when there's a card to show. The
	// translation below fires independently so a learner still gets a
	// meaning check even on an already-correct sentence.
	emit(protocol.ServerEvent{
		Type: protocol.EvCorrection,
		Turn: turn,
		Correction: &protocol.Correction{
			Original:  text,
			Corrected: parsed.Corrected,
			Issues:    parsed.Issues,
		},
	})
	if strings.TrimSpace(parsed.Translation) != "" {
		emit(protocol.ServerEvent{Type: protocol.EvUserTranslation, Turn: turn, Text: parsed.Translation})
	}
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
func (p *Pipeline) translateAssistant(ctx context.Context, turn int, text string, emit Emit) {
	if err := p.acquireTranslationSlot(ctx); err != nil {
		return
	}
	defer p.releaseTranslationSlot()
	raw, err := p.analyze(ctx, translationSystemPrompt(p.FeedbackLang), text, false)
	if err != nil {
		log.Printf("translateAssistant: %v", err)
		return
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return
	}
	emit(protocol.ServerEvent{Type: protocol.EvAssistantTranslation, Turn: turn, Text: raw})
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

// GenerateTitle asks the LLM for a short, descriptive chat-room title from
// its opening exchange, for internal/transport to save once the first reply
// completes (see Handler.generateTitle there) — replacing the raw
// truncated-first-message placeholder store.MySQLStore.SaveTurn sets on
// turn 1. A single fast call (p.LLM/p.ChatModel), not the Analysis
// ensemble+Judge like analyze() uses: a room label is decorative, not
// something a learner's grammar feedback depends on, so it isn't worth
// doubling the LLM cost of every new conversation.
func (p *Pipeline) GenerateTitle(ctx context.Context, userText, assistantText string) (string, error) {
	msgs := []llm.Message{
		{Role: llm.RoleSystem, Content: titleSystemPrompt},
		{Role: llm.RoleUser, Content: renderTitleInput(userText, assistantText)},
	}
	title, err := p.LLM.Complete(ctx, p.ChatModel, msgs, false)
	if err != nil {
		return "", err
	}
	// Small models like to wrap a short title in quotes despite being told
	// not to; strip them so the stored title doesn't carry literal "" marks.
	return strings.Trim(strings.TrimSpace(title), `"“”`), nil
}

const titleSystemPrompt = `Give a short, descriptive title for a chat conversation, based on its
opening exchange. The title summarizes the TOPIC being discussed, not the
learner's exact words.
Rules:
- 2-6 words.
- Plain text: no quotes, no trailing punctuation, no labels like "Title:".
- Write it in the same language the learner used.
- Return ONLY the title, nothing else.`

func renderTitleInput(userText, assistantText string) string {
	var b strings.Builder
	b.WriteString("Learner: " + userText + "\n")
	if assistantText != "" {
		b.WriteString("Assistant: " + assistantText + "\n")
	}
	return b.String()
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
// sentence, span, and suggestion stay in English (the language being learned);
// only the explanation and translation are written in the learner's native
// language so the feedback is easy to understand.
func correctionSystemPrompt(lang string) string {
	native := languageName(lang)
	return fmt.Sprintf(`You are an English writing coach for a %[1]s-speaking learner.
The input is one spoken English sentence, possibly with speech-to-text noise.
It may be preceded by a "Conversation so far" block for context; if so, correct
only the sentence after "Sentence to correct:", and use the context solely to
judge whether that sentence fits (e.g. pronoun/tense agreement with earlier
turns, actually answering what was asked) — never correct the context itself.
Return STRICT JSON only, no prose, in exactly this shape:
{"corrected":"<the sentence rewritten in correct, natural English>",
 "translation":"<natural, colloquial %[1]s translation of the ORIGINAL sentence under correction, so the learner can check it against what they meant to say>",
 "issues":[{"type":"grammar|vocabulary|phrasing|context","span":"<original English text>","suggestion":"<the English fix>","explanation":"<why it is wrong, written in %[1]s, short and kind>"}]}
Rules:
- "corrected", "span", and "suggestion" MUST stay in English.
- "translation" and "explanation" MUST be written in %[1]s.
- "translation" MUST translate the ORIGINAL sentence, not the corrected one.
- Use "context" as the issue type only when the sentence is fine in isolation but doesn't fit the conversation (wrong pronoun/tense given earlier turns, doesn't answer what was actually asked, etc.).
- If the sentence is already correct, return the same text and an empty issues array — still fill in "translation".`, native)
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
