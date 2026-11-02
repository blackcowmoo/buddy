package pipeline

import (
	"context"
	"errors"
	"strings"
	"testing"

	"buddy/server/internal/llm"
)

// assertPhraseHintNonRevealingPrompt pins the prompt rules that keep a hint
// a hint: the whole feature exists to withhold the memorized gloss until the
// learner runs an actual search, so a prompt edit that drops the no-answer
// contract must fail loudly here.
func assertPhraseHintNonRevealingPrompt(t *testing.T, prompt string) {
	t.Helper()
	for _, required := range []string{
		`MUST NOT give or closely paraphrase the expression's translation`,
		"MUST NOT contain wording the learner could memorize",
		`would answer a "what does this mean?"`,
		`Bad: {"hint":"어색한 분위기를 풀어 준다는 뜻이에요."}`,
	} {
		if !strings.Contains(prompt, required) {
			t.Errorf("prompt does not contain %q: %s", required, prompt)
		}
	}
}

func TestPhraseHintParsesHint(t *testing.T) {
	var gotInput, systemPrompt string
	p := &Pipeline{LLM: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
		gotInput = msgs[len(msgs)-1].Content
		systemPrompt = msgs[0].Content
		return `{"hint":"서먹서먹한 첫 만남을 떠올리게 하는 행동과 관련돼요."}`, nil
	}}, ChatModel: "m", FeedbackLang: "ko"}
	got, err := p.PhraseHint(context.Background(), "break the ice", "The host told a joke to break the ice.")
	if err != nil {
		t.Fatalf("PhraseHint() error = %v", err)
	}
	if !strings.Contains(gotInput, "break the ice") || !strings.Contains(gotInput, "The host told a joke") {
		t.Fatalf("input sent to the model = %q, want both the phrase and its context", gotInput)
	}
	want := "서먹서먹한 첫 만남을 떠올리게 하는 행동과 관련돼요."
	if got != want {
		t.Fatalf("PhraseHint() = %q, want %q", got, want)
	}
	assertPhraseHintNonRevealingPrompt(t, systemPrompt)
}

func TestPhraseHintRejectsEmptyHint(t *testing.T) {
	p := &Pipeline{LLM: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
		return `{"hint":"   "}`, nil
	}}, ChatModel: "m"}
	if _, err := p.PhraseHint(context.Background(), "break the ice", "context"); err == nil {
		t.Fatal("expected an error when the model returns a blank hint")
	}
}

func TestPhraseHintRejectsOverlongHint(t *testing.T) {
	p := &Pipeline{LLM: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
		return `{"hint":"` + strings.Repeat("가", maxPhraseHintRunes+1) + `"}`, nil
	}}, ChatModel: "m"}
	if _, err := p.PhraseHint(context.Background(), "break the ice", "context"); err == nil {
		t.Fatal("expected an error when the hint exceeds the length cap")
	}
}

func TestPhraseHintRejectsBadJSON(t *testing.T) {
	p := &Pipeline{LLM: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
		return "not json", nil
	}}, ChatModel: "m"}
	if _, err := p.PhraseHint(context.Background(), "break the ice", "context"); err == nil {
		t.Fatal("expected an error when the model's reply isn't valid JSON")
	}
}

func TestPhraseHintPropagatesLLMError(t *testing.T) {
	p := &Pipeline{LLM: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
		return "", errors.New("down")
	}}, ChatModel: "m"}
	if _, err := p.PhraseHint(context.Background(), "break the ice", "context"); err == nil {
		t.Fatal("expected an error when the LLM call fails")
	}
}
