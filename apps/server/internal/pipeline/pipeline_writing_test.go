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
	got, err := p.GenerateWritingPrompt(context.Background(), "likes coffee; struggles with articles", []string{"전에 쓴 문장입니다."}, "draw-123")
	if err != nil {
		t.Fatal(err)
	}
	if got.Korean == "" || input == "" || !strings.Contains(input, "likes coffee") {
		t.Fatalf("prompt/profile not preserved: got=%+v input=%q", got, input)
	}
	if !strings.Contains(input, "전에 쓴 문장입니다.") {
		t.Fatalf("previous prompts not preserved: input=%q", input)
	}
	if !strings.Contains(input, "draw-123") {
		t.Fatalf("draw ID not included in cache input: input=%q", input)
	}
}
