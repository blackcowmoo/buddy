package pipeline

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"buddy/server/internal/llm"
	"buddy/server/internal/protocol"
	"buddy/server/internal/wordreview"
)

func TestSuggestWordsParsesSuggestions(t *testing.T) {
	var gotInput string
	p := &Pipeline{LLM: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
		gotInput = msgs[len(msgs)-1].Content
		return `{"suggestions":[{"word":"furious","meaning":"화가 나서 참을 수 없는","example":"She was furious when she found out."}]}`, nil
	}}, ChatModel: "m", FeedbackLang: "ko"}
	got, err := p.SuggestWords(context.Background(), "화가 나서 참을 수 없는 느낌")
	if err != nil {
		t.Fatalf("SuggestWords() error = %v", err)
	}
	if gotInput != "화가 나서 참을 수 없는 느낌" {
		t.Fatalf("input sent to the model = %q, want the description verbatim", gotInput)
	}
	want := []protocol.WordSuggestion{{Word: "furious", Meaning: "화가 나서 참을 수 없는", Example: "She was furious when she found out."}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("SuggestWords() = %+v, want %+v", got, want)
	}
}

func TestSuggestWordsPropagatesLLMError(t *testing.T) {
	p := &Pipeline{LLM: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
		return "", errors.New("down")
	}}, ChatModel: "m"}
	if _, err := p.SuggestWords(context.Background(), "설명"); err == nil {
		t.Fatal("expected an error when the LLM call fails")
	}
}

func TestSuggestWordsRejectsBadJSON(t *testing.T) {
	p := &Pipeline{LLM: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
		return "not json", nil
	}}, ChatModel: "m"}
	if _, err := p.SuggestWords(context.Background(), "설명"); err == nil {
		t.Fatal("expected an error when the model's reply isn't valid JSON")
	}
}

// ---- SuggestNewWords() -------------------------------------------------------

func TestSuggestNewWordsParsesSuggestions(t *testing.T) {
	var gotInput string
	p := &Pipeline{LLM: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
		gotInput = msgs[len(msgs)-1].Content
		return `{"suggestions":[{"word":"resilient","meaning":"회복력이 있는","example":"She stayed resilient through the setback."}]}`, nil
	}}, ChatModel: "m", FeedbackLang: "ko"}
	got, err := p.SuggestNewWords(context.Background(), "loves cooking; struggles with articles", []string{"furious", "elated"})
	if err != nil {
		t.Fatalf("SuggestNewWords() error = %v", err)
	}
	if !strings.Contains(gotInput, "loves cooking; struggles with articles") {
		t.Fatalf("input sent to the model = %q, want it to include the learner profile", gotInput)
	}
	if !strings.Contains(gotInput, "furious") || !strings.Contains(gotInput, "elated") {
		t.Fatalf("input sent to the model = %q, want it to include the already-tracked words", gotInput)
	}
	want := []protocol.WordSuggestion{{Word: "resilient", Meaning: "회복력이 있는", Example: "She stayed resilient through the setback."}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("SuggestNewWords() = %+v, want %+v", got, want)
	}
}

func TestSuggestNewWordsHandlesEmptyProfileAndExistingWords(t *testing.T) {
	var gotInput string
	p := &Pipeline{LLM: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
		gotInput = msgs[len(msgs)-1].Content
		return `{"suggestions":[]}`, nil
	}}, ChatModel: "m"}
	if _, err := p.SuggestNewWords(context.Background(), "", nil); err != nil {
		t.Fatalf("SuggestNewWords() error = %v", err)
	}
	if !strings.Contains(gotInput, "(none)") {
		t.Fatalf("input sent to the model = %q, want an explicit (none) placeholder for both blank fields", gotInput)
	}
}

func TestSuggestNewWordsPropagatesLLMError(t *testing.T) {
	p := &Pipeline{LLM: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
		return "", errors.New("down")
	}}, ChatModel: "m"}
	if _, err := p.SuggestNewWords(context.Background(), "profile", nil); err == nil {
		t.Fatal("expected an error when the LLM call fails")
	}
}

// ---- DefineWord() -----------------------------------------------------------

func TestDefineWordParsesDefinition(t *testing.T) {
	var gotInput string
	p := &Pipeline{LLM: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
		gotInput = msgs[len(msgs)-1].Content
		return `{"word":"resilient","meaning":"회복력이 있는","example":"She stayed resilient through the setback."}`, nil
	}}, ChatModel: "m", FeedbackLang: "ko"}
	got, err := p.DefineWord(context.Background(), "resilient", "She stayed resilient through the setback.")
	if err != nil {
		t.Fatalf("DefineWord() error = %v", err)
	}
	if !strings.Contains(gotInput, "resilient") || !strings.Contains(gotInput, "She stayed resilient") {
		t.Fatalf("input sent to the model = %q, want it to include both the word and its context", gotInput)
	}
	want := protocol.WordSuggestion{Word: "resilient", Meaning: "회복력이 있는", Example: "She stayed resilient through the setback."}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("DefineWord() = %+v, want %+v", got, want)
	}
}

func TestDefineWordNormalizesInflectedWordBeforeLookup(t *testing.T) {
	var inputs []string
	p := &Pipeline{LLM: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
		inputs = append(inputs, msgs[len(msgs)-1].Content)
		if len(inputs) == 1 {
			return `{"word":"run"}`, nil
		}
		return `{"word":"run","meaning":"달리다","example":"They run every morning."}`, nil
	}}, ChatModel: "m", FeedbackLang: "ko"}

	got, err := p.DefineWord(context.Background(), "running", "They are running every morning.")
	if err != nil {
		t.Fatalf("DefineWord() error = %v", err)
	}
	if len(inputs) != 2 {
		t.Fatalf("LLM calls = %d, want 2 (resolve then define)", len(inputs))
	}
	if !strings.Contains(inputs[1], "word: run") || strings.Contains(inputs[1], "word: running") {
		t.Fatalf("definition input = %q, want the normalized word only", inputs[1])
	}
	if got.Word != "run" {
		t.Fatalf("result word = %q, want normalized dictionary form", got.Word)
	}
}

func TestDefineWordMeaningsNormalizesInflectedWordBeforeResearch(t *testing.T) {
	var inputs []string
	p := &Pipeline{LLM: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
		inputs = append(inputs, msgs[len(msgs)-1].Content)
		if len(inputs) == 1 {
			return `{"word":"frill"}`, nil
		}
		return `{"suggestions":[{"word":"frill","meaning":"장식","example":"The dress has a frill."}]}`, nil
	}}, ChatModel: "m", FeedbackLang: "ko"}

	got, err := p.DefineWordMeanings(context.Background(), "frills", "The dress has frills.")
	if err != nil {
		t.Fatalf("DefineWordMeanings() error = %v", err)
	}
	if len(inputs) != 2 || !strings.Contains(inputs[1], "word: frill") || strings.Contains(inputs[1], "word: frills") {
		t.Fatalf("lookup inputs = %v, want form resolution followed by base-form lookup", inputs)
	}
	if got[0].Word != "frill" {
		t.Fatalf("result word = %q, want frill", got[0].Word)
	}
}

func TestDefineWordFallsBackToOriginalWhenResolutionFails(t *testing.T) {
	var inputs []string
	p := &Pipeline{LLM: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
		inputs = append(inputs, msgs[len(msgs)-1].Content)
		if len(inputs) == 1 {
			return "not json", nil
		}
		return `{"word":"running","meaning":"달리고 있는","example":"They are running."}`, nil
	}}, ChatModel: "m", FeedbackLang: "ko"}

	got, err := p.DefineWord(context.Background(), "running", "They are running.")
	if err != nil {
		t.Fatalf("DefineWord() error = %v", err)
	}
	if len(inputs) != 2 {
		t.Fatalf("LLM calls = %d, want 2 (failed resolve then original lookup)", len(inputs))
	}
	if !strings.Contains(inputs[1], "word: running") {
		t.Fatalf("fallback input = %q, want the original word", inputs[1])
	}
	if got.Word != "running" {
		t.Fatalf("result word = %q, want original fallback spelling", got.Word)
	}
}

func TestDefineWordPropagatesLLMError(t *testing.T) {
	p := &Pipeline{LLM: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
		return "", errors.New("down")
	}}, ChatModel: "m"}
	if _, err := p.DefineWord(context.Background(), "resilient", "context"); err == nil {
		t.Fatal("expected an error when the LLM call fails")
	}
}

func TestDefineWordRejectsBadJSON(t *testing.T) {
	p := &Pipeline{LLM: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
		return "not json", nil
	}}, ChatModel: "m"}
	if _, err := p.DefineWord(context.Background(), "resilient", "context"); err == nil {
		t.Fatal("expected an error when the model's reply isn't valid JSON")
	}
}

func TestSuggestNewWordsRejectsBadJSON(t *testing.T) {
	p := &Pipeline{LLM: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
		return "not json", nil
	}}, ChatModel: "m"}
	if _, err := p.SuggestNewWords(context.Background(), "profile", nil); err == nil {
		t.Fatal("expected an error when the model's reply isn't valid JSON")
	}
}

// ---- GenerateWordReviewQuestion() -----------------------------------------

func TestGenerateWordReviewQuestionUsesCompleteGrammaticalFormAndStampsVersion(t *testing.T) {
	var systemPrompt, input string
	p := &Pipeline{LLM: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
		systemPrompt = msgs[0].Content
		input = msgs[len(msgs)-1].Content
		return `{"prompt":"The criminals planned the attack in a highly ___ manner.","answer":"organized"}`, nil
	}}, ChatModel: "m", FeedbackLang: "ko"}

	got, err := p.GenerateWordReviewQuestion(context.Background(), "organize", "조직하다", "The criminals planned the attack in a highly organized manner.")
	if err != nil {
		t.Fatalf("GenerateWordReviewQuestion() error = %v", err)
	}
	if got.Version != wordreview.CurrentQuestionVersion || got.Answer != "organized" {
		t.Fatalf("question = %+v, want current version and complete past form", got)
	}
	if strings.Contains(got.Prompt, "___d") {
		t.Fatalf("prompt leaks an inflectional suffix: %q", got.Prompt)
	}
	if !strings.Contains(systemPrompt, "complete grammatical form") || !strings.Contains(systemPrompt, `wrong: "___d"`) {
		t.Fatalf("system prompt does not prohibit partial-form blanks: %q", systemPrompt)
	}
	if !strings.Contains(input, "dictionary word: organize") || !strings.Contains(input, "existing example:") {
		t.Fatalf("generation input = %q, want word and existing example", input)
	}
}

func TestGenerateWordReviewQuestionRejectsMalformedQuestion(t *testing.T) {
	cases := []string{
		`{"prompt":"No blank here.","answer":"organized"}`,
		`{"prompt":"Two ___ blanks ___.","answer":"organized"}`,
		`{"prompt":"They were highly ___d.","answer":"organize"}`,
		`{"prompt":"They ___.","answer":"   "}`,
	}
	for _, response := range cases {
		p := &Pipeline{LLM: &fakeLLM{complete: func([]llm.Message) (string, error) { return response, nil }}, ChatModel: "m"}
		if _, err := p.GenerateWordReviewQuestion(context.Background(), "organize", "정리하다", "They organized it."); err == nil {
			t.Fatalf("response %s: error = nil, want validation failure", response)
		}
	}
}

// ---- VerifyWord() -----------------------------------------------------------

func TestVerifyWordUsesOrderedCascadeAndReturnsJudgeVerdict(t *testing.T) {
	var calls []string
	var analysisInput, judgeInput string
	p := &Pipeline{
		FeedbackLang: "ko",
		LLM: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
			calls = append(calls, "chat")
			return `{"valid":true,"reason":"chat draft"}`, nil
		}},
		Analysis: []Candidate{{Model: "analysis", LLM: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
			calls = append(calls, "analysis")
			analysisInput = msgs[len(msgs)-1].Content
			return `{"valid":true,"reason":"analysis refinement"}`, nil
		}}}},
		Judge: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
			calls = append(calls, "judge")
			judgeInput = msgs[len(msgs)-1].Content
			return `{"valid":false,"reason":"example uses the phrase unnaturally"}`, nil
		}},
	}
	valid, reason, err := p.VerifyWord(context.Background(), "furious", "화가 나서 참을 수 없는", "She was furious.")
	if err != nil {
		t.Fatalf("VerifyWord() error = %v", err)
	}
	if valid || reason != "example uses the phrase unnaturally" {
		t.Fatalf("VerifyWord() = (%v, %q), want Judge's final false verdict", valid, reason)
	}
	if !reflect.DeepEqual(calls, []string{"chat", "analysis", "judge"}) {
		t.Fatalf("stage order = %v, want chat -> analysis -> judge", calls)
	}
	if !strings.Contains(analysisInput, `{"valid":true,"reason":"chat draft"}`) {
		t.Fatalf("Analysis did not receive the Chat draft: %q", analysisInput)
	}
	if !strings.Contains(judgeInput, `{"valid":true,"reason":"chat draft"}`) ||
		!strings.Contains(judgeInput, `{"valid":true,"reason":"analysis refinement"}`) {
		t.Fatalf("Judge did not receive both earlier stages: %q", judgeInput)
	}
}

func TestVerifyWordErrorsWhenNoModelsConfigured(t *testing.T) {
	p := &Pipeline{}
	if _, _, err := p.VerifyWord(context.Background(), "furious", "화가 나서 참을 수 없는", "She was furious."); err == nil {
		t.Fatal("expected an error when no cascade models are configured")
	}
}

// ---- GenerateStudySummary() -----------------------------------------------
