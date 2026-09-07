package pipeline

import (
	"context"
	"errors"
	"strings"
	"testing"

	"buddy/server/internal/llm"
	"buddy/server/internal/protocol"
)

const fakeArticleStudyJSON = `{
	"summary":"Scientists announced a new discovery today.",
	"translation":"과학자들이 오늘 새로운 발견을 발표했습니다.",
	"subQuestions":[
		{"prompt":"어떤 발견이었나요?","options":["정확한 답","틀린 답"],"correctOptionIndex":0,"explanation":"원문의 의미를 정확히 반영하기 때문입니다."},
		{"prompt":"언제 발표됐나요?","options":["오늘","어제"],"correctOptionIndex":0,"explanation":"원문에 today라고 명시되어 있습니다."}
	]
}`

func TestGenerateArticleStudySendsSourceAndParsesResult(t *testing.T) {
	var gotInput string
	p := &Pipeline{
		Analysis: []Candidate{{Model: "m", LLM: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
			gotInput = msgs[len(msgs)-1].Content
			return fakeArticleStudyJSON, nil
		}}}},
	}
	got, err := p.GenerateArticleStudy(context.Background(), "BBC", "Scientists make discovery", "A short snippet about the discovery.")
	if err != nil {
		t.Fatalf("GenerateArticleStudy() error = %v", err)
	}
	if got.Summary != "Scientists announced a new discovery today." {
		t.Errorf("Summary = %q", got.Summary)
	}
	// CorrectOptionIndex's literal value isn't asserted: GenerateArticleStudy
	// reshuffles each sub-question's options (see shuffleSubQuestions) so the
	// answer's position isn't always wherever the LLM originally put it —
	// only that CorrectOptionIndex still points at the one accurate option,
	// wherever shuffling landed it.
	if len(got.SubQuestions) != 2 {
		t.Fatalf("got %d sub-questions, want 2: %+v", len(got.SubQuestions), got)
	}
	if got.SubQuestions[0].Options[got.SubQuestions[0].CorrectOptionIndex] != "정확한 답" {
		t.Fatalf("sub-question 0 = %+v, want CorrectOptionIndex to point at the accurate option", got.SubQuestions[0])
	}
	if got.SubQuestions[1].Options[got.SubQuestions[1].CorrectOptionIndex] != "오늘" {
		t.Fatalf("sub-question 1 = %+v, want CorrectOptionIndex to point at the accurate option", got.SubQuestions[1])
	}
	if !strings.Contains(gotInput, "BBC") || !strings.Contains(gotInput, "Scientists make discovery") {
		t.Fatalf("analyze input missing source/title: %q", gotInput)
	}
}

func TestGenerateArticleStudyStartsWithChatThenUsesAnalysis(t *testing.T) {
	chatCalls := 0
	p := &Pipeline{
		LLM:       &fakeLLM{complete: func(msgs []llm.Message) (string, error) { chatCalls++; return "should not be used", nil }},
		ChatModel: "chat",
		Analysis: []Candidate{{Model: "m", LLM: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
			return fakeArticleStudyJSON, nil
		}}}},
	}
	if _, err := p.GenerateArticleStudy(context.Background(), "BBC", "t", "d"); err != nil {
		t.Fatalf("GenerateArticleStudy() error = %v", err)
	}
	if chatCalls != 1 {
		t.Fatalf("GenerateArticleStudy should start with exactly one Chat draft; got %d calls", chatCalls)
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

func TestGenerateArticleStudyRejectsTooFewSubQuestions(t *testing.T) {
	p := &Pipeline{
		Analysis: []Candidate{{Model: "m", LLM: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
			return `{"summary":"s","subQuestions":[{"prompt":"p","options":["a","b"],"correctOptionIndex":0,"explanation":"e"}]}`, nil
		}}}},
	}
	if _, err := p.GenerateArticleStudy(context.Background(), "BBC", "t", "d"); err == nil {
		t.Fatal("expected an error for a quiz with fewer than articleQuizMinSubQuestions sub-questions")
	}
}

func TestGenerateArticleStudyRejectsWrongOptionCount(t *testing.T) {
	p := &Pipeline{
		Analysis: []Candidate{{Model: "m", LLM: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
			return `{"summary":"s","subQuestions":[
				{"prompt":"p1","options":["a","b","c"],"correctOptionIndex":0,"explanation":"e"},
				{"prompt":"p2","options":["a","b"],"correctOptionIndex":0,"explanation":"e"}
			]}`, nil
		}}}},
	}
	if _, err := p.GenerateArticleStudy(context.Background(), "BBC", "t", "d"); err == nil {
		t.Fatal("expected an error for a sub-question with other than exactly 2 options")
	}
}

func TestGenerateArticleStudyRejectsOutOfRangeCorrectOptionIndex(t *testing.T) {
	p := &Pipeline{
		Analysis: []Candidate{{Model: "m", LLM: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
			return `{"summary":"s","subQuestions":[
				{"prompt":"p1","options":["a","b"],"correctOptionIndex":9,"explanation":"e"},
				{"prompt":"p2","options":["a","b"],"correctOptionIndex":0,"explanation":"e"}
			]}`, nil
		}}}},
	}
	if _, err := p.GenerateArticleStudy(context.Background(), "BBC", "t", "d"); err == nil {
		t.Fatal("expected an error for an out-of-range correctOptionIndex")
	}
}

func TestGenerateArticleStudyRejectsEmptySummary(t *testing.T) {
	p := &Pipeline{
		Analysis: []Candidate{{Model: "m", LLM: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
			return `{"summary":"","subQuestions":[
				{"prompt":"p1","options":["a","b"],"correctOptionIndex":0,"explanation":"e"},
				{"prompt":"p2","options":["a","b"],"correctOptionIndex":0,"explanation":"e"}
			]}`, nil
		}}}},
	}
	if _, err := p.GenerateArticleStudy(context.Background(), "BBC", "t", "d"); err == nil {
		t.Fatal("expected an error for an empty summary")
	}
}

func TestGenerateArticleStudyRejectsEmptyPromptOrOptionOrExplanation(t *testing.T) {
	cases := []struct {
		name string
		json string
	}{
		{"empty prompt", `{"summary":"s","subQuestions":[{"prompt":"","options":["a","b"],"correctOptionIndex":0,"explanation":"e"},{"prompt":"p","options":["a","b"],"correctOptionIndex":0,"explanation":"e"}]}`},
		{"empty option", `{"summary":"s","subQuestions":[{"prompt":"p","options":["","b"],"correctOptionIndex":0,"explanation":"e"},{"prompt":"p","options":["a","b"],"correctOptionIndex":0,"explanation":"e"}]}`},
		{"empty explanation", `{"summary":"s","subQuestions":[{"prompt":"p","options":["a","b"],"correctOptionIndex":0,"explanation":""},{"prompt":"p","options":["a","b"],"correctOptionIndex":0,"explanation":"e"}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &Pipeline{
				Analysis: []Candidate{{Model: "m", LLM: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
					return tc.json, nil
				}}}},
			}
			if _, err := p.GenerateArticleStudy(context.Background(), "BBC", "t", "d"); err == nil {
				t.Fatalf("%s: expected an error", tc.name)
			}
		})
	}
}

// TestShuffleSubQuestionsPreservesCorrectAnswer runs the shuffle many times
// (it's randomized) to guard the one invariant that matters: whatever
// position the correct option ends up at, CorrectOptionIndex must point at
// it, and both original options must still be present.
func TestShuffleSubQuestionsPreservesCorrectAnswer(t *testing.T) {
	original := protocol.ArticleStudy{
		SubQuestions: []protocol.ArticleSubQuestion{
			{Prompt: "p", Options: []string{"correct one", "wrong one"}, CorrectOptionIndex: 0},
		},
	}
	sawIndexOne := false
	for i := 0; i < 50; i++ {
		// shuffleSubQuestions mutates Options in place, so pass a fresh copy
		// of the sub-question slice/options each iteration.
		fresh := protocol.ArticleStudy{SubQuestions: []protocol.ArticleSubQuestion{
			{Prompt: "p", Options: []string{"correct one", "wrong one"}, CorrectOptionIndex: 0},
		}}
		got := shuffleSubQuestions(fresh)
		q := got.SubQuestions[0]
		if len(q.Options) != 2 {
			t.Fatalf("shuffleSubQuestions() Options = %v, want 2 entries", q.Options)
		}
		if q.Options[q.CorrectOptionIndex] != "correct one" {
			t.Fatalf("shuffleSubQuestions() = %+v, CorrectOptionIndex must point at the correct option", q)
		}
		gotSet := map[string]bool{q.Options[0]: true, q.Options[1]: true}
		for _, c := range original.SubQuestions[0].Options {
			if !gotSet[c] {
				t.Fatalf("shuffleSubQuestions() dropped option %q: got %v", c, q.Options)
			}
		}
		if q.CorrectOptionIndex == 1 {
			sawIndexOne = true
		}
	}
	if !sawIndexOne {
		t.Error("shuffleSubQuestions() always left CorrectOptionIndex at 0 across 50 runs — shuffling isn't actually happening")
	}
}
