package pipeline

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync/atomic"
	"time"

	"buddy/server/internal/llm"
	"buddy/server/internal/protocol"
)

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

// translateAssistant runs the assistant's plain native-language translation
// through Chat -> Analysis -> Judge, with a plain-text (not JSON) prompt since
// there's nothing else to parse out. Unlike correction, it does not expose the
// Chat draft: a translation is secondary enrichment rather than something the
// learner is waiting on to continue, and showing it early would either mutate
// already-read message content or require a second unread-notification surface.
// Only the terminal result is emitted once.
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
	// Keep the exact Chat output as the durable handoff, but do not emit it.
	chatDraft, chatErr := p.chatDraft(ctx, translationSystemPrompt(p.FeedbackLang), text, false)
	if chatErr != nil {
		log.Printf("translateAssistant: chat draft: %v", chatErr)
	}

	onResult := func(translation string) {
		// analyzeFromDraft() never succeeds with a blank result (a candidate's own
		// empty output is filtered out before it can win), so this event
		// always carries real text — the client's pending/spinner state
		// (see App.tsx) treats this event's arrival as the "translation
		// finished" signal.
		emit(protocol.ServerEvent{Type: protocol.EvAssistantTranslation, Turn: turn, Text: translation})
	}
	if p.TranslateHook != nil {
		p.TranslateHook(ctx, userID, sessionID, turn, text, chatDraft, onResult)
		return
	}
	translation, err := p.AnalyzeTranslationFromDraft(ctx, text, chatDraft)
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

// AnalyzeTranslationFromDraft continues the cascade from the hidden Chat
// translation draft. It acquires the same low-priority serialized translation
// slot as AnalyzeTranslation, but does not call Chat again.
func (p *Pipeline) AnalyzeTranslationFromDraft(ctx context.Context, text, chatDraft string) (string, error) {
	if err := p.acquireTranslationSlot(ctx); err != nil {
		return "", err
	}
	defer p.releaseTranslationSlot()
	raw, err := p.analyzeFromDraft(ctx, translationSystemPrompt(p.FeedbackLang), text, false, chatDraft)
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
	raw, err := p.chatDraft(ctx, translationSystemPrompt(p.FeedbackLang), text, false)
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
