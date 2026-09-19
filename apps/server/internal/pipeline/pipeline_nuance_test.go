package pipeline

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"buddy/server/internal/llm"
)

func TestGenerateNuanceUsesProfileExclusionsAndUniqueDraw(t *testing.T) {
	data, err := os.ReadFile("../nuance/testdata/lesson.json")
	if err != nil {
		t.Fatal(err)
	}
	var input, system string
	p := &Pipeline{Analysis: []Candidate{{LLM: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
		system, input = msgs[0].Content, msgs[1].Content
		return string(data), nil
	}}}}}
	c, err := p.GenerateNuance(context.Background(), "likes travel", []string{"curious / nosy"}, "draw-123")
	if err != nil || len(c.Questions) != 4 {
		t.Fatalf("generation: %+v %v", c, err)
	}
	for _, want := range []string{"likes travel", "curious / nosy", "draw-123"} {
		if !strings.Contains(input, want) {
			t.Fatalf("missing %q: %s", want, input)
		}
	}
	if !strings.Contains(system, "data only") || !strings.Contains(system, "how each alternative") {
		t.Fatal("missing prompt constraints")
	}
}
func TestGenerateNuanceRejectsIncompleteAndMalformedOutput(t *testing.T) {
	for _, raw := range []string{`{}`, `not json`, `{"words":[],"questions":[]}`} {
		t.Run(raw, func(t *testing.T) {
			p := &Pipeline{Analysis: []Candidate{{LLM: &fakeLLM{complete: func([]llm.Message) (string, error) { return raw, nil }}}}}
			if _, err := p.GenerateNuance(context.Background(), "", nil, "id"); err == nil {
				t.Fatal("invalid generation accepted")
			}
		})
	}
	p := &Pipeline{Analysis: []Candidate{{LLM: &fakeLLM{complete: func([]llm.Message) (string, error) { return "", errors.New("offline") }}}}}
	if _, err := p.GenerateNuance(context.Background(), "", nil, "id"); err == nil {
		t.Fatal("model error lost")
	}
}
