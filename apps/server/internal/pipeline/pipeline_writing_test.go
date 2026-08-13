package pipeline

import (
	"context"
	"strings"
	"testing"

	"buddy/server/internal/llm"
)

func TestGenerateWritingPromptUsesLearnerProfileAndParsesJSON(t *testing.T) {
	var input string
	p := &Pipeline{Analysis: []Candidate{{LLM: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
		input = msgs[1].Content
		return `{"korean":"주말에는 새로운 카페를 찾아가 보려고 해요."}`, nil
	}}}}}
	got, err := p.GenerateWritingPrompt(context.Background(), "likes coffee; struggles with articles")
	if err != nil {
		t.Fatal(err)
	}
	if got.Korean == "" || input == "" || !strings.Contains(input, "likes coffee") {
		t.Fatalf("prompt/profile not preserved: got=%+v input=%q", got, input)
	}
}
