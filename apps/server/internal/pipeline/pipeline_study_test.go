package pipeline

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"buddy/server/internal/llm"
	"buddy/server/internal/protocol"
)

func TestGenerateStudySummarySendsIssueDetails(t *testing.T) {
	var gotInput string
	p := &Pipeline{
		Analysis: []Candidate{{Model: "m", LLM: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
			gotInput = msgs[len(msgs)-1].Content
			return `{"sentences":[
				{"english":"You keep dropping third-person -s.","translation":"꾸준히 3인칭 단수 -s를 놓치고 있어요."},
				{"english":"Keep practicing!","translation":"계속 연습해봐요!"}
			]}`, nil
		}}}},
	}
	issues := []StudyIssue{
		{Text: "He go to school.", Issue: protocol.Issue{Type: "grammar", Span: "go", Suggestion: "goes", Explanation: "subject-verb agreement"}},
	}
	got, err := p.GenerateStudySummary(context.Background(), issues)
	if err != nil {
		t.Fatalf("GenerateStudySummary() error = %v", err)
	}
	want := []protocol.StudySummarySentence{
		{English: "You keep dropping third-person -s.", Translation: "꾸준히 3인칭 단수 -s를 놓치고 있어요."},
		{English: "Keep practicing!", Translation: "계속 연습해봐요!"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("GenerateStudySummary() = %+v, want %+v", got, want)
	}
	if !strings.Contains(gotInput, "He go to school.") || !strings.Contains(gotInput, "subject-verb agreement") {
		t.Fatalf("analyze input missing issue details: %q", gotInput)
	}
}

// TestGenerateStudySummaryStartsWithChatThenUsesAnalysis guards the common
// cascade: the internal Chat draft must be produced exactly once before the
// Analysis refinement, even though only the terminal result is published.
func TestGenerateStudySummaryStartsWithChatThenUsesAnalysis(t *testing.T) {
	chatCalls := 0
	p := &Pipeline{
		LLM:       &fakeLLM{complete: func(msgs []llm.Message) (string, error) { chatCalls++; return "should not be used", nil }},
		ChatModel: "chat",
		Analysis: []Candidate{{Model: "m", LLM: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
			return `{"sentences":[{"english":"summary","translation":"요약"}]}`, nil
		}}}},
	}
	if _, err := p.GenerateStudySummary(context.Background(), []StudyIssue{{Text: "x", Issue: protocol.Issue{Type: "grammar"}}}); err != nil {
		t.Fatalf("GenerateStudySummary() error = %v", err)
	}
	if chatCalls != 1 {
		t.Fatalf("GenerateStudySummary should start with exactly one Chat draft; got %d calls", chatCalls)
	}
}

func TestGenerateStudySummaryPropagatesAnalyzeError(t *testing.T) {
	p := &Pipeline{
		Analysis: []Candidate{{Model: "m", LLM: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
			return "", errors.New("down")
		}}}},
	}
	if _, err := p.GenerateStudySummary(context.Background(), []StudyIssue{{Text: "x", Issue: protocol.Issue{Type: "grammar"}}}); err == nil {
		t.Fatal("expected an error when every candidate fails")
	}
}

// ---- GenerateStudyQuiz() ---------------------------------------------------

func TestGenerateStudyQuizSendsIssueDetails(t *testing.T) {
	var gotInput string
	var gotPrompt string
	p := &Pipeline{
		Analysis: []Candidate{{Model: "m", LLM: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
			gotInput = msgs[len(msgs)-1].Content
			gotPrompt = msgs[0].Content
			return `{"questions":[
				{"prompt":"She ___ to the store every day.","answer":"goes","translation":"그녀는 매일 가게에 가요.","explanation":"third person singular needs -s","explanationTranslation":"3인칭 단수는 -s가 필요해요"}
			]}`, nil
		}}}},
	}
	issues := []StudyIssue{
		{Text: "He go to school.", Issue: protocol.Issue{Type: "grammar", Span: "go", Suggestion: "goes", Explanation: "subject-verb agreement"}},
	}
	got, err := p.GenerateStudyQuiz(context.Background(), issues)
	if err != nil {
		t.Fatalf("GenerateStudyQuiz() error = %v", err)
	}
	want := []protocol.QuizQuestion{
		{
			Prompt:                 "She ___ to the store every day.",
			Answer:                 "goes",
			Translation:            "그녀는 매일 가게에 가요.",
			Explanation:            "third person singular needs -s",
			ExplanationTranslation: "3인칭 단수는 -s가 필요해요",
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("GenerateStudyQuiz() = %+v, want %+v", got, want)
	}
	if !strings.Contains(gotInput, "He go to school.") || !strings.Contains(gotInput, "subject-verb agreement") {
		t.Fatalf("analyze input missing issue details: %q", gotInput)
	}
	if !strings.Contains(gotPrompt, "including the grammatical form required by the sentence") {
		t.Fatalf("quiz prompt does not require the sentence's grammatical form: %q", gotPrompt)
	}
}

// TestGenerateStudyQuizStartsWithChatThenUsesAnalysis mirrors the summary
// test: the hidden Chat draft must start the chain before Analysis refines it.
func TestGenerateStudyQuizStartsWithChatThenUsesAnalysis(t *testing.T) {
	chatCalls := 0
	p := &Pipeline{
		LLM:       &fakeLLM{complete: func(msgs []llm.Message) (string, error) { chatCalls++; return "should not be used", nil }},
		ChatModel: "chat",
		Analysis: []Candidate{{Model: "m", LLM: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
			return `{"questions":[{"prompt":"He ___ to school.","answer":"goes","translation":"그는 학교에 가요.","explanation":"x","explanationTranslation":"y"}]}`, nil
		}}}},
	}
	if _, err := p.GenerateStudyQuiz(context.Background(), []StudyIssue{{Text: "x", Issue: protocol.Issue{Type: "grammar"}}}); err != nil {
		t.Fatalf("GenerateStudyQuiz() error = %v", err)
	}
	if chatCalls != 1 {
		t.Fatalf("GenerateStudyQuiz should start with exactly one Chat draft; got %d calls", chatCalls)
	}
}

func TestGenerateStudyQuizPropagatesAnalyzeError(t *testing.T) {
	p := &Pipeline{
		Analysis: []Candidate{{Model: "m", LLM: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
			return "", errors.New("down")
		}}}},
	}
	if _, err := p.GenerateStudyQuiz(context.Background(), []StudyIssue{{Text: "x", Issue: protocol.Issue{Type: "grammar"}}}); err == nil {
		t.Fatal("expected an error when every candidate fails")
	}
}

func TestGenerateStudyQuizPropagatesBadJSON(t *testing.T) {
	p := &Pipeline{
		Analysis: []Candidate{{Model: "m", LLM: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
			return "not json", nil
		}}}},
	}
	if _, err := p.GenerateStudyQuiz(context.Background(), []StudyIssue{{Text: "x", Issue: protocol.Issue{Type: "grammar"}}}); err == nil {
		t.Fatal("expected an error when the LLM returns non-JSON")
	}
}

// ---- CheckQuizAnswer() -----------------------------------------------------------

func TestCheckQuizAnswerParsesVerdictAndSendsDetails(t *testing.T) {
	var gotInput string
	p := &Pipeline{LLM: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
		gotInput = msgs[len(msgs)-1].Content
		return `{"correct": true}`, nil
	}}, ChatModel: "m"}
	got, err := p.CheckQuizAnswer(context.Background(), "He ___ to school.", "goes", []string{"walks"}, "commutes")
	if err != nil {
		t.Fatalf("CheckQuizAnswer() error = %v", err)
	}
	if !got {
		t.Fatalf("CheckQuizAnswer() = false, want true")
	}
	for _, want := range []string{"He ___ to school.", "goes", "walks", "commutes"} {
		if !strings.Contains(gotInput, want) {
			t.Fatalf("input sent to the model missing %q: %q", want, gotInput)
		}
	}
}

func TestCheckQuizAnswerUsesChatModelNotAnalysisEnsemble(t *testing.T) {
	analysisCalls := 0
	p := &Pipeline{
		Analysis: []Candidate{{Model: "m", LLM: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
			analysisCalls++
			return "should not be used", nil
		}}}},
		LLM:       &fakeLLM{complete: func(msgs []llm.Message) (string, error) { return `{"correct": false}`, nil }},
		ChatModel: "chat",
	}
	if _, err := p.CheckQuizAnswer(context.Background(), "He ___ to school.", "goes", nil, "walks"); err != nil {
		t.Fatalf("CheckQuizAnswer() error = %v", err)
	}
	if analysisCalls != 0 {
		t.Fatalf("CheckQuizAnswer should use the chat model, not the Analysis ensemble; got %d analysis calls", analysisCalls)
	}
}

func TestRefineQuizAnswerPassesChatVerdictThroughAnalysisAndJudge(t *testing.T) {
	var analysisInput, judgeInput string
	p := &Pipeline{
		Analysis: []Candidate{{Model: "analysis", LLM: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
			analysisInput = msgs[len(msgs)-1].Content
			return `{"correct": false}`, nil
		}}}},
		Judge: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
			judgeInput = msgs[len(msgs)-1].Content
			return `{"correct": false}`, nil
		}},
		JudgeModel: "judge",
	}
	got, err := p.RefineQuizAnswerFromDraft(context.Background(), "He ___ home.", "went", nil, "goed", `{"correct": true}`)
	if err != nil {
		t.Fatalf("RefineQuizAnswerFromDraft() error = %v", err)
	}
	if got {
		t.Fatal("RefineQuizAnswerFromDraft() = true, want Judge's false verdict")
	}
	if !strings.Contains(analysisInput, "Chat draft to refine:\n{\"correct\": true}") {
		t.Fatalf("Analysis input = %q, want exact Chat verdict", analysisInput)
	}
	if !strings.Contains(judgeInput, "Chat draft:\n{\"correct\": true}") || !strings.Contains(judgeInput, `{"correct": false}`) {
		t.Fatalf("Judge input = %q, want Chat and Analysis verdicts", judgeInput)
	}
}

func TestCheckQuizAnswerPropagatesLLMError(t *testing.T) {
	p := &Pipeline{LLM: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
		return "", errors.New("down")
	}}, ChatModel: "m"}
	if _, err := p.CheckQuizAnswer(context.Background(), "p", "a", nil, "b"); err == nil {
		t.Fatal("expected an error when the LLM call fails")
	}
}

func TestCheckQuizAnswerRejectsBadJSON(t *testing.T) {
	p := &Pipeline{LLM: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
		return "not json", nil
	}}, ChatModel: "m"}
	if _, err := p.CheckQuizAnswer(context.Background(), "p", "a", nil, "b"); err == nil {
		t.Fatal("expected an error when the model's reply isn't valid JSON")
	}
}
