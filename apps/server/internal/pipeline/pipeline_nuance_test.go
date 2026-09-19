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
	if !strings.Contains(system, "forbidden word combinations") || !strings.Contains(system, "Choose a different combination") {
		t.Fatal("missing duplicate rejection guidance")
	}
	for name, constraints := range map[string][]string{
		"shared sense rather than related vocabulary": {
			"share the same core meaning in the specific sense being taught",
			"A shared Korean translation or a related topic alone is not enough",
			"Prefer two words over forcing a third that is not interchangeable",
			"Do not contrast borrow/lend",
		},
		"every option fits every question": {
			"ALL words must fit the blank exactly without changing inflection, articles, prepositions, or surrounding syntax",
			"a grammatical, plausible sentence with the same core meaning in the given situation",
			"Do not use grammar errors, fixed collocations, idioms, different word senses, or factual contradictions",
		},
		"preference follows speaker intention": {
			"context must specify the audience, relationship, setting, or desired tone",
			"alternatives remain valid ways to convey the same basic message",
			"rewrite the question or choose another word set",
		},
		"explanations compare substitutions without declaring them wrong": {
			"Each Korean explanation must state the shared meaning",
			"how each alternative changes the nuance when substituted into this same sentence",
			"Do not call alternatives wrong in meaning or ungrammatical",
			"this does not imply interchangeability in every other sense or expression",
		},
		"model checks substitutions before returning": {
			"silently substitute every word into every question",
			"Rewrite any question that fails these checks",
		},
	} {
		t.Run(name, func(t *testing.T) {
			for _, want := range constraints {
				if !strings.Contains(system, want) {
					t.Errorf("missing generation constraint %q", want)
				}
			}
		})
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
