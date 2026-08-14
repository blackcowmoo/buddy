package pipeline

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"buddy/server/internal/llm"
	"buddy/server/internal/protocol"
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

// ---- VerifyWord() -----------------------------------------------------------

func TestVerifyWordPassesWhenAllJudgesAgreeValid(t *testing.T) {
	p := &Pipeline{
		FeedbackLang: "ko",
		Analysis: []Candidate{{Model: "m", LLM: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
			return `{"valid":true,"reason":""}`, nil
		}}}},
	}
	valid, reason, err := p.VerifyWord(context.Background(), "furious", "화가 나서 참을 수 없는", "She was furious.")
	if err != nil {
		t.Fatalf("VerifyWord() error = %v", err)
	}
	if !valid {
		t.Errorf("valid = false, want true when every judge agrees")
	}
	if reason != "" {
		t.Errorf("reason = %q, want empty on a pass", reason)
	}
}

// TestVerifyWordRoundRobinsToReachMinimumJudgesWithOneCandidate is the
// direct regression test for the "even a single-local-model deployment
// still gets several independent judgments" requirement — see
// minWordVerifyJudges' doc.
func TestVerifyWordRoundRobinsToReachMinimumJudgesWithOneCandidate(t *testing.T) {
	var calls int32
	p := &Pipeline{
		Analysis: []Candidate{{Model: "only-model", LLM: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
			atomic.AddInt32(&calls, 1)
			return `{"valid":true,"reason":""}`, nil
		}}}},
	}
	if _, _, err := p.VerifyWord(context.Background(), "furious", "화가 나서 참을 수 없는", "She was furious."); err != nil {
		t.Fatalf("VerifyWord() error = %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != minWordVerifyJudges {
		t.Errorf("judge calls = %d, want %d (round-robined through the one configured candidate)", got, minWordVerifyJudges)
	}
}

// TestVerifyWordRejectsOnAnyDissent guards the "unanimous, not majority or
// synthesized" requirement: a single judge saying invalid must reject the
// word outright, using that judge's own reason, unlike analyze()'s
// Judge-reconciliation step for other tasks.
func TestVerifyWordRejectsOnAnyDissent(t *testing.T) {
	var calls int32
	p := &Pipeline{
		Analysis: []Candidate{{Model: "m", LLM: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
			n := atomic.AddInt32(&calls, 1)
			if n == 2 {
				return `{"valid":false,"reason":"not a real word"}`, nil
			}
			return `{"valid":true,"reason":""}`, nil
		}}}},
	}
	valid, reason, err := p.VerifyWord(context.Background(), "xyzzy", "존재하지 않는 단어", "xyzzy the door.")
	if err != nil {
		t.Fatalf("VerifyWord() error = %v", err)
	}
	if valid {
		t.Error("valid = true, want false when any judge dissents")
	}
	if reason != "not a real word" {
		t.Errorf("reason = %q, want the dissenting judge's own reason", reason)
	}
}

// TestVerifyWordErrorsWhenTooFewJudgesSucceed guards the "infrastructure
// hiccup must not masquerade as a rejection" requirement: if fewer than 2
// judge calls succeed (e.g. most configured endpoints are down), the word
// must stay Pending for asyncjob's reaper to retry — not get marked
// Rejected over a transient failure.
func TestVerifyWordErrorsWhenTooFewJudgesSucceed(t *testing.T) {
	var calls int32
	p := &Pipeline{
		Analysis: []Candidate{{Model: "m", LLM: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
			n := atomic.AddInt32(&calls, 1)
			if n == 1 {
				return `{"valid":true,"reason":""}`, nil
			}
			return "", errors.New("model unreachable")
		}}}},
	}
	if _, _, err := p.VerifyWord(context.Background(), "furious", "화가 나서 참을 수 없는", "She was furious."); err == nil {
		t.Fatal("expected an error when fewer than 2 judge calls succeed")
	}
}

func TestVerifyWordErrorsWhenNoCandidatesConfigured(t *testing.T) {
	p := &Pipeline{}
	if _, _, err := p.VerifyWord(context.Background(), "furious", "화가 나서 참을 수 없는", "She was furious."); err == nil {
		t.Fatal("expected an error when no Analysis candidates are configured")
	}
}

// ---- GenerateStudySummary() -----------------------------------------------
