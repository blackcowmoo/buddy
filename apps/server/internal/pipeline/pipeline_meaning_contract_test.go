package pipeline

import (
	"context"
	"strings"
	"testing"

	"buddy/server/internal/llm"
)

// TestMeaningContractSharedByGenerationAndVerification pins the guarantee the
// save-time stamping (wordreview.GeneratedSaver) relies on: a meaning is
// skipped by the cleanup migration because the generating prompt already
// states the dictionary-meaning contract, so every vocabulary prompt that
// WRITES a meaning and every prompt that later VERIFIES one must embed the
// byte-identical dictionaryMeaningRules block for the learner's language.
// Changing the block's content is a contract change: bump
// wordreview.CurrentMeaningVersion so StartMeaningCleanup re-verifies saved
// glosses; changing wording here without that bump breaks both sides at once.
func TestMeaningContractSharedByGenerationAndVerification(t *testing.T) {
	for _, lang := range []string{"ko", "ja"} {
		native := languageName(lang)
		rules := dictionaryMeaningRules(native)

		generation := map[string]string{
			"suggest":    wordSuggestionSystemPrompt(lang),
			"define":     wordDefineSystemPrompt(lang),
			"auto":       wordAutoSuggestSystemPrompt(lang),
			"correction": correctionSystemPrompt(lang),
		}
		for name, prompt := range generation {
			if !strings.Contains(prompt, rules) {
				t.Errorf("%s prompt does not embed the exact dictionary-meaning rules for %s", name, native)
			}
		}

		var captured []string
		model := &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
			prompt := msgs[0].Content
			captured = append(captured, prompt)
			if strings.Contains(prompt, "List up to 6 distinct common meanings") {
				return `{"suggestions":[{"word":"bank","meaning":"은행","example":"I went to the bank."}]}`, nil
			}
			return `{"sameSense":true,"meaning":"운영하다"}`, nil
		}}
		p := &Pipeline{FeedbackLang: lang, LLM: model, Analysis: []Candidate{{LLM: model}}, Judge: model}
		if _, err := p.NeedsWordMeaningCleanup(context.Background(), "run", "경영하다", "She runs the company."); err != nil {
			t.Fatal(err)
		}
		if _, err := p.NormalizeWordMeaning(context.Background(), "run", "경영하다", "She runs the company."); err != nil {
			t.Fatal(err)
		}
		if _, err := p.generateWordMeanings(context.Background(), "bank"); err != nil {
			t.Fatal(err)
		}
		if len(captured) == 0 {
			t.Fatalf("%s: no verification prompts captured", native)
		}
		for i, prompt := range captured {
			if !strings.Contains(prompt, rules) {
				t.Errorf("verification prompt %d does not embed the exact dictionary-meaning rules for %s", i, native)
			}
		}
	}
}
