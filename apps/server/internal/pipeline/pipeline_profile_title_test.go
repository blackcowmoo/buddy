package pipeline

import (
	"context"
	"errors"
	"strings"
	"testing"

	"buddy/server/internal/llm"
)

// titleTranscript is a small helper building the []llm.Message shape
// GenerateTitle now takes (a conversation window), so title tests read the
// same as they did with the old (userText, assistantText string) signature.
func titleTranscript(userText, assistantText string) []llm.Message {
	return []llm.Message{
		{Role: llm.RoleUser, Content: userText},
		{Role: llm.RoleAssistant, Content: assistantText},
	}
}

func TestGenerateTitleSendsUserAndAssistantText(t *testing.T) {
	var gotInput string
	p := &Pipeline{LLM: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
		gotInput = msgs[len(msgs)-1].Content
		return "Hiking Trip Plans", nil
	}}, ChatModel: "m"}
	got, err := p.GenerateTitle(context.Background(), titleTranscript("I went hiking last weekend.", "That sounds fun! Where did you go?"))
	if err != nil {
		t.Fatalf("GenerateTitle() error = %v", err)
	}
	if got != "Hiking Trip Plans" {
		t.Fatalf("GenerateTitle() = %q, want the model's title verbatim", got)
	}
	if !strings.Contains(gotInput, "I went hiking last weekend.") || !strings.Contains(gotInput, "Where did you go?") {
		t.Fatalf("input should include both the learner's and assistant's text, got %q", gotInput)
	}
}

func TestGenerateTitleTrimsQuotesAndWhitespace(t *testing.T) {
	p := &Pipeline{LLM: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
		return `  "Weekend Hiking Trip"  `, nil
	}}, ChatModel: "m"}
	got, err := p.GenerateTitle(context.Background(), titleTranscript("hi", "hello"))
	if err != nil {
		t.Fatalf("GenerateTitle() error = %v", err)
	}
	if got != "Weekend Hiking Trip" {
		t.Fatalf("GenerateTitle() = %q, want surrounding quotes/whitespace stripped", got)
	}
}

func TestGenerateTitlePublishesOnlyJudgeResult(t *testing.T) {
	p := &Pipeline{
		LLM: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
			return "Chat Hiking", nil
		}},
		ChatModel: "chat",
		Analysis: []Candidate{{Model: "analysis", LLM: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
			if !strings.Contains(msgs[len(msgs)-1].Content, "Chat Hiking") {
				t.Fatalf("Analysis did not receive the Chat title: %+v", msgs)
			}
			return "Analysis Hiking", nil
		}}}},
		Judge: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
			input := msgs[len(msgs)-1].Content
			if !strings.Contains(input, "Chat Hiking") || !strings.Contains(input, "Analysis Hiking") {
				t.Fatalf("Judge did not receive every prior title stage: %+v", msgs)
			}
			return "Judge Hiking", nil
		}},
		JudgeModel: "judge",
	}

	got, err := p.GenerateTitle(context.Background(), titleTranscript("I went hiking.", "Nice!"))
	if err != nil {
		t.Fatalf("GenerateTitle() error = %v", err)
	}
	if got != "Judge Hiking" {
		t.Fatalf("GenerateTitle() = %q, want terminal Judge title", got)
	}
}

func TestGenerateTitlePropagatesLLMError(t *testing.T) {
	p := &Pipeline{LLM: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
		return "", errors.New("down")
	}}, ChatModel: "m"}
	if _, err := p.GenerateTitle(context.Background(), titleTranscript("hi", "hello")); err == nil {
		t.Fatal("expected an error when the LLM call fails")
	}
}

// ---- SuggestWords() -----------------------------------------------------------

func TestUpdateLearnerProfileSendsPreviousProfileAndNewSummary(t *testing.T) {
	var gotInput string
	p := &Pipeline{
		Analysis: []Candidate{{Model: "m", LLM: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
			gotInput = msgs[len(msgs)-1].Content
			return "  struggles with third-person -s; enjoys talking about hiking  ", nil
		}}}},
	}
	got, err := p.UpdateLearnerProfile(context.Background(), "enjoys talking about hiking", "여전히 3인칭 단수 -s를 놓치고 있어요")
	if err != nil {
		t.Fatalf("UpdateLearnerProfile() error = %v", err)
	}
	if got != "struggles with third-person -s; enjoys talking about hiking" {
		t.Fatalf("UpdateLearnerProfile() = %q, want trimmed profile", got)
	}
	if !strings.Contains(gotInput, "enjoys talking about hiking") || !strings.Contains(gotInput, "여전히 3인칭 단수 -s를 놓치고 있어요") {
		t.Fatalf("analyze input missing previous profile or new summary: %q", gotInput)
	}
}

func TestUpdateLearnerProfileHandlesEmptyPreviousProfile(t *testing.T) {
	var gotInput string
	p := &Pipeline{
		Analysis: []Candidate{{Model: "m", LLM: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
			gotInput = msgs[len(msgs)-1].Content
			return "first profile", nil
		}}}},
	}
	if _, err := p.UpdateLearnerProfile(context.Background(), "", "first session's wrap-up"); err != nil {
		t.Fatalf("UpdateLearnerProfile() error = %v", err)
	}
	if !strings.Contains(gotInput, "(none)") {
		t.Fatalf("analyze input should mark an empty previous profile as (none): %q", gotInput)
	}
}

func TestUpdateLearnerProfilePropagatesAnalyzeError(t *testing.T) {
	p := &Pipeline{
		Analysis: []Candidate{{Model: "m", LLM: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
			return "", errors.New("down")
		}}}},
	}
	if _, err := p.UpdateLearnerProfile(context.Background(), "prev", "new"); err == nil {
		t.Fatal("expected an error when every candidate fails")
	}
}
