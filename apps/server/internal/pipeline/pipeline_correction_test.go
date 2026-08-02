package pipeline

import (
	"context"
	"errors"
	"strings"
	"testing"

	"buddy/server/internal/llm"
	"buddy/server/internal/protocol"
)

func TestCorrectEmitsEventWhenChanged(t *testing.T) {
	fixture := func(msgs []llm.Message) (string, error) {
		return `{"corrected":"I like pizza.","translation":"저는 피자를 좋아해요.","issues":[{"type":"grammar","span":"I likes","suggestion":"I like","explanation":"subject-verb agreement error","explanationTranslation":"수 일치 오류"}]}`, nil
	}
	p := &Pipeline{
		LLM:       &fakeLLM{complete: fixture},
		ChatModel: "m",
		Analysis:  []Candidate{{Model: "m", LLM: &fakeLLM{complete: fixture}}},
	}
	var got []protocol.ServerEvent
	p.correct(context.Background(), "alex", "sess-1", 1, "I likes pizza", "", func(ev protocol.ServerEvent) { got = append(got, ev) })

	if len(got) != 2 || got[0].Type != protocol.EvCorrection || got[1].Type != protocol.EvUserTranslation {
		t.Fatalf("expected a correction event followed by a translation event, got %+v", got)
	}
	if got[0].Correction.Corrected != "I like pizza." {
		t.Fatalf("Correction.Corrected = %q", got[0].Correction.Corrected)
	}
	if got[0].Correction.Issues[0].Explanation != "subject-verb agreement error" {
		t.Fatalf("explanation not passed through: %+v", got[0].Correction.Issues[0])
	}
	if got[0].Correction.Issues[0].ExplanationTranslation != "수 일치 오류" {
		t.Fatalf("explanationTranslation not passed through: %+v", got[0].Correction.Issues[0])
	}
	if got[1].Text != "저는 피자를 좋아해요." || got[1].Turn != 1 {
		t.Fatalf("translation event wrong: %+v", got[1])
	}
}

func TestCorrectEmitsEventWithNoIssuesWhenAlreadyCorrect(t *testing.T) {
	fixture := func(msgs []llm.Message) (string, error) {
		return `{"corrected":"I like pizza.","issues":[]}`, nil
	}
	p := &Pipeline{
		LLM:       &fakeLLM{complete: fixture},
		ChatModel: "m",
		Analysis:  []Candidate{{Model: "m", LLM: &fakeLLM{complete: fixture}}},
	}
	var got []protocol.ServerEvent
	p.correct(context.Background(), "alex", "sess-1", 1, "I like pizza.", "", func(ev protocol.ServerEvent) { got = append(got, ev) })

	if len(got) != 1 || got[0].Type != protocol.EvCorrection {
		t.Fatalf("expected exactly one correction event (no translation in the fixture), got %+v", got)
	}
	if got[0].Correction.Corrected != "I like pizza." || len(got[0].Correction.Issues) != 0 {
		t.Fatalf("expected a no-issue correction event, got %+v", got[0].Correction)
	}
}

// TestCorrectEmitsTranslationEvenWhenAlreadyCorrect guards the meaning-check
// use case: a learner should still get a translation to compare against what
// they meant to say even when the sentence needed no grammar teaching.
func TestCorrectEmitsTranslationEvenWhenAlreadyCorrect(t *testing.T) {
	fixture := func(msgs []llm.Message) (string, error) {
		return `{"corrected":"I like pizza.","translation":"저는 피자를 좋아해요.","issues":[]}`, nil
	}
	p := &Pipeline{
		LLM:       &fakeLLM{complete: fixture},
		ChatModel: "m",
		Analysis:  []Candidate{{Model: "m", LLM: &fakeLLM{complete: fixture}}},
	}
	var got []protocol.ServerEvent
	p.correct(context.Background(), "alex", "sess-1", 1, "I like pizza.", "", func(ev protocol.ServerEvent) { got = append(got, ev) })

	if len(got) != 2 || got[0].Type != protocol.EvCorrection || got[1].Type != protocol.EvUserTranslation {
		t.Fatalf("expected a no-issue correction event followed by a translation event, got %+v", got)
	}
	if len(got[0].Correction.Issues) != 0 {
		t.Fatalf("expected no issues, got %+v", got[0].Correction)
	}
	if got[1].Text != "저는 피자를 좋아해요." {
		t.Fatalf("translation text = %q", got[1].Text)
	}
}

// TestCorrectEmitsFailedOnMalformedJSON guards the fix for a real
// confusion: a malformed model response used to just drop the event
// entirely, leaving the frontend's grammar spinner indistinguishable from
// "still checking" forever. It must now emit a definitive EvCorrection with
// Failed set and no Correction, so the client can show a distinct failed
// state instead (see protocol.ServerEvent.Failed).
func TestCorrectEmitsFailedOnMalformedJSON(t *testing.T) {
	fixture := func(msgs []llm.Message) (string, error) {
		return "not json", nil
	}
	p := &Pipeline{
		LLM:       &fakeLLM{complete: fixture},
		ChatModel: "m",
		Analysis:  []Candidate{{Model: "m", LLM: &fakeLLM{complete: fixture}}},
	}
	var got []protocol.ServerEvent
	p.correct(context.Background(), "alex", "sess-1", 1, "whatever", "", func(ev protocol.ServerEvent) { got = append(got, ev) })
	if len(got) != 1 || got[0].Type != protocol.EvCorrection {
		t.Fatalf("expected exactly one correction event, got %+v", got)
	}
	if !got[0].Failed || got[0].Correction != nil {
		t.Fatalf("expected Failed=true and no Correction, got %+v", got[0])
	}
}

// TestCorrectEmitsFailedOnLLMError is TestCorrectEmitsFailedOnMalformedJSON's
// counterpart for the LLM call itself failing (e.g. the backend down).
func TestCorrectEmitsFailedOnLLMError(t *testing.T) {
	fixture := func(msgs []llm.Message) (string, error) {
		return "", errors.New("down")
	}
	p := &Pipeline{
		LLM:       &fakeLLM{complete: fixture},
		ChatModel: "m",
		Analysis:  []Candidate{{Model: "m", LLM: &fakeLLM{complete: fixture}}},
	}
	var got []protocol.ServerEvent
	p.correct(context.Background(), "alex", "sess-1", 1, "whatever", "", func(ev protocol.ServerEvent) { got = append(got, ev) })
	if len(got) != 1 || got[0].Type != protocol.EvCorrection {
		t.Fatalf("expected exactly one correction event, got %+v", got)
	}
	if !got[0].Failed || got[0].Correction != nil {
		t.Fatalf("expected Failed=true and no Correction, got %+v", got[0])
	}
}

// TestCorrectEmitsFastThenRefinedWhenDifferent guards the two-stage flow
// itself: the FAST pass (p.LLM/p.ChatModel) must emit first, and once the
// slower REFINE ensemble lands on a different answer, correct() must patch
// it in with a second event rather than silently dropping the upgrade.
func TestCorrectEmitsFastThenRefinedWhenDifferent(t *testing.T) {
	p := &Pipeline{
		LLM:       &fakeLLM{complete: func(msgs []llm.Message) (string, error) { return `{"corrected":"I likes pizza.","issues":[]}`, nil }},
		ChatModel: "chat",
		Analysis: []Candidate{{Model: "m", LLM: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
			return `{"corrected":"I like pizza.","issues":[{"type":"grammar","span":"likes","suggestion":"like","explanation":"subject-verb agreement","explanationTranslation":"수 일치"}]}`, nil
		}}}},
	}
	var got []protocol.ServerEvent
	p.correct(context.Background(), "alex", "sess-1", 1, "I likes pizza", "", func(ev protocol.ServerEvent) { got = append(got, ev) })

	if len(got) != 2 || got[0].Type != protocol.EvCorrection || got[1].Type != protocol.EvCorrection {
		t.Fatalf("expected two correction events (fast, then refined), got %+v", got)
	}
	if got[0].Correction.Corrected != "I likes pizza." {
		t.Fatalf("fast event should carry the fast pass's own (uncorrected) result, got %+v", got[0].Correction)
	}
	if got[1].Correction.Corrected != "I like pizza." || len(got[1].Correction.Issues) != 1 {
		t.Fatalf("refined event should carry the ensemble's better result, got %+v", got[1].Correction)
	}
}

// TestCorrectSkipsSecondEmitWhenRefineAgreesWithFast is
// TestCorrectEmitsFastThenRefinedWhenDifferent's counterpart: when the
// ensemble lands on the exact same answer the fast pass already showed,
// correct() must not emit a second, identical card.
func TestCorrectSkipsSecondEmitWhenRefineAgreesWithFast(t *testing.T) {
	fixture := func(msgs []llm.Message) (string, error) {
		return `{"corrected":"I like pizza.","issues":[]}`, nil
	}
	p := &Pipeline{
		LLM:       &fakeLLM{complete: fixture},
		ChatModel: "chat",
		Analysis:  []Candidate{{Model: "m", LLM: &fakeLLM{complete: fixture}}},
	}
	var got []protocol.ServerEvent
	p.correct(context.Background(), "alex", "sess-1", 1, "I like pizza.", "", func(ev protocol.ServerEvent) { got = append(got, ev) })

	if len(got) != 1 {
		t.Fatalf("expected exactly one correction event when fast and refined agree, got %+v", got)
	}
}

// TestCorrectFastFailureFallsBackToRefineOnlyNoFailedEvent: if the fast pass
// errors but the ensemble still succeeds, correct() must not report a
// failure — the learner ends up with a real (if slightly delayed) result,
// not a false "check failed" state.
func TestCorrectFastFailureFallsBackToRefineOnlyNoFailedEvent(t *testing.T) {
	p := &Pipeline{
		LLM:       &fakeLLM{complete: func(msgs []llm.Message) (string, error) { return "", errors.New("chat model down") }},
		ChatModel: "chat",
		Analysis: []Candidate{{Model: "m", LLM: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
			return `{"corrected":"I like pizza.","issues":[]}`, nil
		}}}},
	}
	var got []protocol.ServerEvent
	p.correct(context.Background(), "alex", "sess-1", 1, "I like pizza.", "", func(ev protocol.ServerEvent) { got = append(got, ev) })

	if len(got) != 1 || got[0].Failed {
		t.Fatalf("expected exactly one non-failed correction event from the ensemble, got %+v", got)
	}
	if got[0].Correction == nil || got[0].Correction.Corrected != "I like pizza." {
		t.Fatalf("expected the ensemble's result, got %+v", got[0])
	}
}

// TestCorrectEmitsFailedOnlyWhenBothFastAndRefineFail is
// TestCorrectFastFailureFallsBackToRefineOnlyNoFailedEvent's counterpart:
// only when neither pass produced anything usable should the learner see a
// failed state.
func TestCorrectEmitsFailedOnlyWhenBothFastAndRefineFail(t *testing.T) {
	p := &Pipeline{
		LLM:       &fakeLLM{complete: func(msgs []llm.Message) (string, error) { return "", errors.New("chat model down") }},
		ChatModel: "chat",
		Analysis: []Candidate{{Model: "m", LLM: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
			return "", errors.New("analysis down")
		}}}},
	}
	var got []protocol.ServerEvent
	p.correct(context.Background(), "alex", "sess-1", 1, "I like pizza.", "", func(ev protocol.ServerEvent) { got = append(got, ev) })

	if len(got) != 1 || !got[0].Failed || got[0].Correction != nil {
		t.Fatalf("expected exactly one Failed=true correction event, got %+v", got)
	}
}

// ---- translateAssistant() -------------------------------------------------------

func TestCorrectSendsBareSentenceWhenNoContext(t *testing.T) {
	var gotInput string
	fixture := func(msgs []llm.Message) (string, error) {
		gotInput = msgs[len(msgs)-1].Content
		return `{"corrected":"ok","issues":[]}`, nil
	}
	p := &Pipeline{
		LLM:       &fakeLLM{complete: fixture},
		ChatModel: "m",
		Analysis:  []Candidate{{Model: "m", LLM: &fakeLLM{complete: fixture}}},
	}
	p.correct(context.Background(), "alex", "sess-1", 1, "ok", "", func(protocol.ServerEvent) {})
	if gotInput != "ok" {
		t.Fatalf("with no context, analyze input should be the bare sentence, got %q", gotInput)
	}
}

func TestCorrectFoldsContextInFrontOfSentence(t *testing.T) {
	var gotInput string
	fixture := func(msgs []llm.Message) (string, error) {
		gotInput = msgs[len(msgs)-1].Content
		return `{"corrected":"I am 20 years old.","issues":[]}`, nil
	}
	p := &Pipeline{
		LLM:       &fakeLLM{complete: fixture},
		ChatModel: "m",
		Analysis:  []Candidate{{Model: "m", LLM: &fakeLLM{complete: fixture}}},
	}
	ctxMsg := "Conversation so far:\nassistant: How old are you?\n"
	p.correct(context.Background(), "alex", "sess-1", 1, "I am 20 years old.", ctxMsg, func(protocol.ServerEvent) {})

	if !strings.Contains(gotInput, "How old are you?") {
		t.Fatalf("analyze input missing the context block: %q", gotInput)
	}
	if !strings.HasSuffix(gotInput, "Sentence to correct:\nI am 20 years old.") {
		t.Fatalf("the sentence under correction should be delimited at the end: %q", gotInput)
	}
}

func TestRenderCorrectionContext(t *testing.T) {
	if got := renderCorrectionContext("", nil); got != "" {
		t.Fatalf("expected empty string with no summary/prior turns, got %q", got)
	}
	got := renderCorrectionContext("likes hiking", []llm.Message{
		{Role: llm.RoleUser, Content: "hi"},
		{Role: llm.RoleAssistant, Content: "hello"},
	})
	if !strings.Contains(got, "likes hiking") || !strings.Contains(got, "hi") || !strings.Contains(got, "hello") {
		t.Fatalf("renderCorrectionContext missing content: %q", got)
	}
}

// ---- CorrectWithContext() -------------------------------------------------

func TestCorrectWithContextSendsBareSentenceWhenNoPriorTurns(t *testing.T) {
	var gotInput string
	p := &Pipeline{
		Analysis: []Candidate{{Model: "m", LLM: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
			gotInput = msgs[len(msgs)-1].Content
			return `{"corrected":"I like pizza.","translation":"저는 피자를 좋아해요.","issues":[]}`, nil
		}}}},
	}
	corrected, issues, translation, err := p.CorrectWithContext(context.Background(), nil, "I likes pizza.")
	if err != nil {
		t.Fatalf("CorrectWithContext() error = %v", err)
	}
	if corrected != "I like pizza." || len(issues) != 0 || translation != "저는 피자를 좋아해요." {
		t.Fatalf("CorrectWithContext() = (%q, %v, %q)", corrected, issues, translation)
	}
	if gotInput != "I likes pizza." {
		t.Fatalf("input with no prior turns should be the bare sentence, got %q", gotInput)
	}
}

func TestCorrectWithContextFoldsPriorTurnsIntoInput(t *testing.T) {
	var gotInput string
	p := &Pipeline{
		Analysis: []Candidate{{Model: "m", LLM: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
			gotInput = msgs[len(msgs)-1].Content
			return `{"corrected":"I am 20 years old.","issues":[]}`, nil
		}}}},
	}
	prior := []llm.Message{
		{Role: llm.RoleAssistant, Content: "How old are you?"},
	}
	if _, _, _, err := p.CorrectWithContext(context.Background(), prior, "I am 20 years old."); err != nil {
		t.Fatalf("CorrectWithContext() error = %v", err)
	}
	if !strings.Contains(gotInput, "How old are you?") {
		t.Fatalf("input should fold in prior turns for context, got %q", gotInput)
	}
	if !strings.HasSuffix(gotInput, "Sentence to correct:\nI am 20 years old.") {
		t.Fatalf("input should label the sentence under correction, got %q", gotInput)
	}
}

func TestCorrectWithContextPropagatesAnalyzeError(t *testing.T) {
	p := &Pipeline{
		Analysis: []Candidate{{Model: "m", LLM: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
			return "", errors.New("down")
		}}}},
	}
	if _, _, _, err := p.CorrectWithContext(context.Background(), nil, "whatever"); err == nil {
		t.Fatal("expected an error when every candidate fails")
	}
}

func TestRenderCorrectionInput(t *testing.T) {
	if got := renderCorrectionInput("", "just the sentence"); got != "just the sentence" {
		t.Fatalf("no context should pass the sentence through unchanged, got %q", got)
	}
	got := renderCorrectionInput("CTX BLOCK\n", "the sentence")
	if !strings.HasPrefix(got, "CTX BLOCK") || !strings.HasSuffix(got, "Sentence to correct:\nthe sentence") {
		t.Fatalf("combined input wrong: %q", got)
	}
}

// ---- analyze() -----------------------------------------------------------------

func TestCorrectionSystemPromptNamesTargetLanguage(t *testing.T) {
	p := correctionSystemPrompt("ko")
	if !strings.Contains(p, "Korean") {
		t.Fatalf("prompt should mention Korean: %s", p)
	}
}

func TestCorrectionSystemPromptFlagsUnnaturalPhrasingNotJustGrammar(t *testing.T) {
	p := correctionSystemPrompt("ko")
	if !strings.Contains(p, "natural, idiomatic English, not merely grammatically parseable") {
		t.Fatalf("prompt should require flagging unnatural-but-grammatical phrasing, got: %s", p)
	}
}

// TestCorrectionSystemPromptRequiresCorrectThenDiff guards the fix for a
// one-shot-guess prompt: issues must now be derived from an explicit
// before/after comparison against "corrected", not judged independently of
// it, so every explanation traces back to a real textual change.
func TestCorrectionSystemPromptRequiresCorrectThenDiff(t *testing.T) {
	p := correctionSystemPrompt("ko")
	if !strings.Contains(p, "Work in this order") {
		t.Fatalf("prompt should force a correct-then-diff order, got: %s", p)
	}
	if !strings.Contains(p, `"span" MUST be verbatim text from the original sentence, and "suggestion" MUST be verbatim text from "corrected"`) {
		t.Fatalf("prompt should require span/suggestion to be grounded in an actual diff, got: %s", p)
	}
}
