package pipeline

import (
	"context"
	"fmt"
	"log"
	"strings"

	"buddy/server/internal/llm"
	"buddy/server/internal/protocol"
)

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
// every fast back-and-forth exchange. ctx is still threaded through each
// per-model queue and analyze() (rather than dropping it) so a caller that
// legitimately wants early cancellation — like internal/backfill's long-lived
// worker ctx via TranslateWithContext below — still gets it.
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
// learner's native language. Each Chat, Analysis, and Judge call uses the
// queue for its own LLM, so unrelated models can proceed independently.
// Exported so transport's queue-backed TranslateHook implementation reuses
// the exact same call translateAssistant() uses directly by default.
func (p *Pipeline) AnalyzeTranslation(ctx context.Context, text string) (string, error) {
	raw, err := p.analyze(ctx, translationSystemPrompt(p.FeedbackLang), text, false)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(raw), nil
}

// AnalyzeTranslationFromDraft continues the cascade from the hidden Chat
// translation draft. It does not call Chat again; each remaining model call
// uses that model's independent queue.
func (p *Pipeline) AnalyzeTranslationFromDraft(ctx context.Context, text, chatDraft string) (string, error) {
	raw, err := p.analyzeFromDraft(ctx, translationSystemPrompt(p.FeedbackLang), text, false, chatDraft)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(raw), nil
}

// AnalyzeTranslationFast is AnalyzeTranslation's FAST-track counterpart: one
// call to the chat model (p.LLM/p.ChatModel) instead of the analysis
// ensemble, mirroring AnalyzeCorrectionFast — see translateAssistant()'s
// two-stage flow. The Chat model's own queue applies to this call. A blank
// (whitespace-only) result is treated as an error, matching analyze()'s own
// "no blank winners" contract.
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
