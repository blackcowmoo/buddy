package pipeline

import (
	"context"
	"strings"
	"testing"

	"buddy/server/internal/llm"
)

func TestVocabularyPromptsShareDictionaryExamples(t *testing.T) {
	for name, prompt := range map[string]string{
		"define":     wordDefineSystemPrompt("ko"),
		"suggest":    wordSuggestionSystemPrompt("ko"),
		"auto":       wordAutoSuggestSystemPrompt("ko"),
		"correction": correctionSystemPrompt("ko"),
	} {
		t.Run(name, func(t *testing.T) { assertArticleMemorizationMeaningPrompt(t, prompt) })
	}
	if strings.Contains(dictionaryMeaningRules("Japanese"), "생산 시설") {
		t.Fatal("Korean examples leaked into another feedback language")
	}
}

func TestMeaningResearchReceivesDictionaryExamples(t *testing.T) {
	p := &Pipeline{FeedbackLang: "ko", LLM: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
		if msgs[0].Content == wordFormSystemPrompt() {
			return `{"word":"bank"}`, nil
		}
		assertArticleMemorizationMeaningPrompt(t, msgs[0].Content)
		if !strings.Contains(msgs[0].Content, "each distinct sense in a separate entry") {
			t.Fatal("missing sense separation")
		}
		return `{"suggestions":[{"word":"bank","meaning":"은행","example":"I went to the bank."},{"word":"bank","meaning":"강둑","example":"They sat on the bank."}]}`, nil
	}}}
	got, err := p.DefineWordMeanings(context.Background(), "bank", "I went to the bank.")
	if err != nil || len(got) != 2 || got[0].Meaning != "은행" || got[1].Meaning != "강둑" {
		t.Fatalf("results=%+v err=%v", got, err)
	}
}

func TestNormalizeWordMeaningPreservesSenseThroughCascade(t *testing.T) {
	var calls []string
	model := func(stage, response string) *fakeLLM {
		return &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
			calls = append(calls, stage)
			assertArticleMemorizationMeaningPrompt(t, msgs[0].Content)
			if !strings.Contains(msgs[len(msgs)-1].Content, "She runs the company.") {
				t.Fatal("missing original example")
			}
			return response, nil
		}}
	}
	p := &Pipeline{FeedbackLang: "ko", LLM: model("chat", `{"sameSense":true,"meaning":"운영하다"}`), Analysis: []Candidate{{LLM: model("analysis", `{"sameSense":true,"meaning":"운영하다"}`)}}, Judge: model("judge", `{"sameSense":true,"meaning":" 경영하다, 운영하다 "}`)}
	got, err := p.NormalizeWordMeaning(context.Background(), "run", "문맥상 회사를 운영한다는 뜻", "She runs the company.")
	if err != nil || got != "경영하다, 운영하다" || strings.Join(calls, ",") != "chat,analysis,judge" {
		t.Fatalf("meaning=%q calls=%v err=%v", got, calls, err)
	}
}

func TestNormalizeWordMeaningRejectsUncertainOrMalformedResult(t *testing.T) {
	for _, response := range []string{`{"sameSense":false,"meaning":"은행"}`, `{"meaning":"은행"}`, `{"sameSense":true,"meaning":" "}`, `invalid`} {
		t.Run(response, func(t *testing.T) {
			p := &Pipeline{LLM: &fakeLLM{complete: func([]llm.Message) (string, error) { return response, nil }}}
			if _, err := p.NormalizeWordMeaning(context.Background(), "bank", "unclear", ""); err == nil {
				t.Fatal("uncertain/malformed output accepted")
			}
		})
	}
}
