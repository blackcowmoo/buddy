package pipeline

import (
	"context"
	"errors"
	"strings"
	"testing"

	"buddy/server/internal/llm"
	"buddy/server/internal/protocol"
)

func TestGenerateArticleStudySendsSourceAndParsesResult(t *testing.T) {
	var gotInput string
	p := &Pipeline{
		Analysis: []Candidate{{Model: "m", LLM: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
			gotInput = msgs[len(msgs)-1].Content
			return `{
				"summary":"Scientists announced a new discovery today.",
				"choices":["정확한 해석입니다.","틀린 해석 1입니다.","틀린 해석 2입니다.","틀린 해석 3입니다."],
				"correctIndex":0,
				"explanation":"원문의 의미를 정확히 반영하기 때문입니다."
			}`, nil
		}}}},
	}
	got, err := p.GenerateArticleStudy(context.Background(), "BBC", "Scientists make discovery", "A short snippet about the discovery.")
	if err != nil {
		t.Fatalf("GenerateArticleStudy() error = %v", err)
	}
	if got.Summary != "Scientists announced a new discovery today." {
		t.Errorf("Summary = %q", got.Summary)
	}
	// CorrectIndex's literal value isn't asserted: GenerateArticleStudy
	// reshuffles Choices (see shuffleChoices) so the answer's position isn't
	// always wherever the LLM originally put it — only that CorrectIndex
	// still points at the one accurate choice, wherever shuffling landed it.
	if len(got.Choices) != 4 || got.Choices[got.CorrectIndex] != "정확한 해석입니다." {
		t.Fatalf("got = %+v, want CorrectIndex to point at the accurate choice", got)
	}
	if !strings.Contains(gotInput, "BBC") || !strings.Contains(gotInput, "Scientists make discovery") {
		t.Fatalf("analyze input missing source/title: %q", gotInput)
	}
}

func TestGenerateArticleStudyUsesAnalysisEnsembleNotChatModel(t *testing.T) {
	chatCalls := 0
	p := &Pipeline{
		LLM:       &fakeLLM{complete: func(msgs []llm.Message) (string, error) { chatCalls++; return "should not be used", nil }},
		ChatModel: "chat",
		Analysis: []Candidate{{Model: "m", LLM: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
			return `{"summary":"s","choices":["a","b","c","d"],"correctIndex":1,"explanation":"e"}`, nil
		}}}},
	}
	if _, err := p.GenerateArticleStudy(context.Background(), "BBC", "t", "d"); err != nil {
		t.Fatalf("GenerateArticleStudy() error = %v", err)
	}
	if chatCalls != 0 {
		t.Fatalf("GenerateArticleStudy should use the Analysis ensemble, not the chat model directly; got %d chat calls", chatCalls)
	}
}

func TestGenerateArticleStudyPropagatesAnalyzeError(t *testing.T) {
	p := &Pipeline{
		Analysis: []Candidate{{Model: "m", LLM: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
			return "", errors.New("down")
		}}}},
	}
	if _, err := p.GenerateArticleStudy(context.Background(), "BBC", "t", "d"); err == nil {
		t.Fatal("expected an error when every candidate fails")
	}
}

func TestGenerateArticleStudyRejectsWrongChoiceCount(t *testing.T) {
	p := &Pipeline{
		Analysis: []Candidate{{Model: "m", LLM: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
			return `{"summary":"s","choices":["a","b","c"],"correctIndex":0,"explanation":"e"}`, nil
		}}}},
	}
	if _, err := p.GenerateArticleStudy(context.Background(), "BBC", "t", "d"); err == nil {
		t.Fatal("expected an error for a quiz with the wrong number of choices")
	}
}

func TestGenerateArticleStudyRejectsOutOfRangeCorrectIndex(t *testing.T) {
	p := &Pipeline{
		Analysis: []Candidate{{Model: "m", LLM: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
			return `{"summary":"s","choices":["a","b","c","d"],"correctIndex":9,"explanation":"e"}`, nil
		}}}},
	}
	if _, err := p.GenerateArticleStudy(context.Background(), "BBC", "t", "d"); err == nil {
		t.Fatal("expected an error for an out-of-range correctIndex")
	}
}

func TestGenerateArticleStudyRejectsEmptySummary(t *testing.T) {
	p := &Pipeline{
		Analysis: []Candidate{{Model: "m", LLM: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
			return `{"summary":"","choices":["a","b","c","d"],"correctIndex":0,"explanation":"e"}`, nil
		}}}},
	}
	if _, err := p.GenerateArticleStudy(context.Background(), "BBC", "t", "d"); err == nil {
		t.Fatal("expected an error for an empty summary")
	}
}

// TestShuffleChoicesPreservesCorrectAnswer runs the shuffle many times (it's
// randomized) to guard the one invariant that matters: whatever position
// the correct choice ends up at, CorrectIndex must point at it, and every
// original choice must still be present exactly once.
func TestShuffleChoicesPreservesCorrectAnswer(t *testing.T) {
	original := protocol.ArticleStudy{
		Choices:      []string{"correct one", "wrong a", "wrong b", "wrong c"},
		CorrectIndex: 0,
	}
	sawIndexOtherThanZero := false
	for i := 0; i < 50; i++ {
		got := shuffleChoices(original)
		if len(got.Choices) != 4 {
			t.Fatalf("shuffleChoices() Choices = %v, want 4 entries", got.Choices)
		}
		if got.Choices[got.CorrectIndex] != "correct one" {
			t.Fatalf("shuffleChoices() = %+v, CorrectIndex must point at the correct choice", got)
		}
		gotSet := map[string]bool{}
		for _, c := range got.Choices {
			gotSet[c] = true
		}
		for _, c := range original.Choices {
			if !gotSet[c] {
				t.Fatalf("shuffleChoices() dropped choice %q: got %v", c, got.Choices)
			}
		}
		if got.CorrectIndex != 0 {
			sawIndexOtherThanZero = true
		}
	}
	if !sawIndexOtherThanZero {
		t.Error("shuffleChoices() always left CorrectIndex at 0 across 50 runs — shuffling isn't actually happening")
	}
}
