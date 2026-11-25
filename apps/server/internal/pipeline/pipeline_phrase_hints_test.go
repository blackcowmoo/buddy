package pipeline

import (
	"context"
	"errors"
	"strings"
	"testing"

	"buddy/server/internal/llm"
)

const hintsTestSummary = "The host told a joke to break the ice at the summit."

// TestSuggestPhraseHintsParsesBatch guards the happy path and the two
// contracts the persisted map depends on: keys are the exact stored phrase
// strings (the browser looks hints up by tapped phrase, so a renamed key is
// a miss no reader can recover from), and the batch prompt carries the same
// pinned no-answer rules as the single-hint prompt.
func TestSuggestPhraseHintsParsesBatch(t *testing.T) {
	var gotInput, systemPrompt string
	p := &Pipeline{LLM: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
		gotInput = msgs[len(msgs)-1].Content
		systemPrompt = msgs[0].Content
		return `{"hints":[{"phrase":"break the ice","hint":"서먹서먹한 첫 만남을 떠올리게 하는 행동과 관련돼요."},{"phrase":"told a joke","hint":"누군가를 웃게 만들려고 한 말이에요."}]}`, nil
	}}, ChatModel: "m", FeedbackLang: "ko"}
	got, err := p.SuggestPhraseHints(context.Background(), hintsTestSummary, []string{"break the ice", "told a joke"})
	if err != nil {
		t.Fatalf("SuggestPhraseHints() error = %v", err)
	}
	if len(got) != 2 || got["break the ice"] != "서먹서먹한 첫 만남을 떠올리게 하는 행동과 관련돼요." || got["told a joke"] == "" {
		t.Fatalf("SuggestPhraseHints() = %v, want hints keyed by the exact stored phrases", got)
	}
	for _, want := range []string{"break the ice", "told a joke", hintsTestSummary} {
		if !strings.Contains(gotInput, want) {
			t.Errorf("input sent to the model = %q, want it to contain %q", gotInput, want)
		}
	}
	// Same hint contract as PhraseHint — the batch prompt may wrap it
	// differently, but every pinned rule sentence must still be there.
	for _, required := range []string{
		`MUST NOT give or closely paraphrase the expression's translation`,
		"MUST NOT contain wording the learner could memorize",
		`would answer a "what does this mean?"`,
		`Bad: {"hint":"어색한 분위기를 풀어 준다는 뜻이에요."}`,
	} {
		if !strings.Contains(systemPrompt, required) {
			t.Errorf("batch hint prompt does not contain %q: %s", required, systemPrompt)
		}
	}
}

// TestSuggestPhraseHintsDropsUnknownPhrases guards that a hallucinated
// expression never reaches the stored map — a key no stored phrase matches
// would be a hint nobody can look up, and worse, it would let the model
// attach hints to wording that isn't in the article at all. The valid
// entries survive: partial coverage still beats failing the whole batch.
func TestSuggestPhraseHintsDropsUnknownPhrases(t *testing.T) {
	p := &Pipeline{LLM: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
		return `{"hints":[{"phrase":"break the ice","hint":"첫 만남과 관련돼요."},{"phrase":"spill the beans","hint":"숨긴 말을 밝혀요."}]}`, nil
	}}, ChatModel: "m"}
	got, err := p.SuggestPhraseHints(context.Background(), hintsTestSummary, []string{"break the ice"})
	if err != nil {
		t.Fatalf("SuggestPhraseHints() error = %v", err)
	}
	if len(got) != 1 || got["break the ice"] != "첫 만남과 관련돼요." {
		t.Fatalf("SuggestPhraseHints() = %v, want only the requested phrase", got)
	}
}

// TestSuggestPhraseHintsToleratesKeyWhitespace guards that trivial model
// drift in echoing a key back (case, padding) still maps to the stored
// phrase — but the key stored stays the exact requested string.
func TestSuggestPhraseHintsToleratesKeyWhitespace(t *testing.T) {
	p := &Pipeline{LLM: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
		return `{"hints":[{"phrase":" Break  The  Ice ","hint":"첫 만남과 관련돼요."}]}`, nil
	}}, ChatModel: "m"}
	got, err := p.SuggestPhraseHints(context.Background(), hintsTestSummary, []string{"break the ice"})
	if err != nil {
		t.Fatalf("SuggestPhraseHints() error = %v", err)
	}
	if len(got) != 1 || got["break the ice"] == "" {
		t.Fatalf("SuggestPhraseHints() = %v, want the hint under the exact stored key", got)
	}
}

// TestSuggestPhraseHintsDropsInvalidHints mirrors parsePhraseHint's per-hint
// rules: blank and overlong entries are dropped, not truncated into
// half-answers, while the good ones survive.
func TestSuggestPhraseHintsDropsInvalidHints(t *testing.T) {
	long := strings.Repeat("가", maxPhraseHintRunes+1)
	p := &Pipeline{LLM: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
		return `{"hints":[{"phrase":"break the ice","hint":"첫 만남과 관련돼요."},{"phrase":"told a joke","hint":"   "},{"phrase":"summit","hint":"` + long + `"}]}`, nil
	}}, ChatModel: "m"}
	got, err := p.SuggestPhraseHints(context.Background(), hintsTestSummary, []string{"break the ice", "told a joke", "summit"})
	if err != nil {
		t.Fatalf("SuggestPhraseHints() error = %v", err)
	}
	if len(got) != 1 || got["break the ice"] == "" {
		t.Fatalf("SuggestPhraseHints() = %v, want only the valid hint kept", got)
	}
}

// TestSuggestPhraseHintsErrorsWhenNothingSurvives guards the "empty means
// not generated" persistence contract: a batch whose every entry is invalid
// must error (so the durable job retries) rather than cache an empty map.
func TestSuggestPhraseHintsErrorsWhenNothingSurvives(t *testing.T) {
	p := &Pipeline{LLM: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
		return `{"hints":[{"phrase":"not a stored phrase","hint":"무엇인지 알 수 없어요."}]}`, nil
	}}, ChatModel: "m"}
	if _, err := p.SuggestPhraseHints(context.Background(), hintsTestSummary, []string{"break the ice"}); err == nil {
		t.Fatal("expected an error when no valid hint for any stored phrase survives")
	}
}

func TestSuggestPhraseHintsRejectsEmptyPhraseList(t *testing.T) {
	called := false
	p := &Pipeline{LLM: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
		called = true
		return `{}`, nil
	}}, ChatModel: "m"}
	if _, err := p.SuggestPhraseHints(context.Background(), hintsTestSummary, nil); err == nil {
		t.Fatal("expected an error when there are no phrases to hint")
	}
	if called {
		t.Fatal("empty phrase list must not reach the LLM at all")
	}
}

func TestSuggestPhraseHintsRejectsBadJSON(t *testing.T) {
	p := &Pipeline{LLM: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
		return "not json", nil
	}}, ChatModel: "m"}
	if _, err := p.SuggestPhraseHints(context.Background(), hintsTestSummary, []string{"break the ice"}); err == nil {
		t.Fatal("expected an error when the model's reply isn't valid JSON")
	}
}

func TestSuggestPhraseHintsPropagatesLLMError(t *testing.T) {
	p := &Pipeline{LLM: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
		return "", errors.New("down")
	}}, ChatModel: "m"}
	if _, err := p.SuggestPhraseHints(context.Background(), hintsTestSummary, []string{"break the ice"}); err == nil {
		t.Fatal("expected an error when the LLM call fails")
	}
}
