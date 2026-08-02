package pipeline

import (
	"context"
	"fmt"
	"log"
	"strings"

	"buddy/server/internal/llm"
	"buddy/server/internal/protocol"
)

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
	parsed, err := parseJSON[struct {
		Corrected   string           `json:"corrected"`
		Translation string           `json:"translation"`
		Issues      []protocol.Issue `json:"issues"`
	}](raw, "")
	if err != nil {
		return "", nil, "", err
	}
	return parsed.Corrected, parsed.Issues, parsed.Translation, nil
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
