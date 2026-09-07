package pipeline

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"buddy/server/internal/llm"
	"buddy/server/internal/session"
)

func TestCompactNoopBelowThreshold(t *testing.T) {
	sess := session.New("sys")
	sess.AppendUser("u1")
	sess.AppendAssistant("a1")
	calls := 0
	p := &Pipeline{
		Analysis: []Candidate{{Model: "m", LLM: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
			calls++
			return "should not be called", nil
		}}}},
		MaxHistoryMessages: 20,
	}

	p.compact(sess)

	if calls != 0 {
		t.Fatalf("expected no LLM call below threshold, got %d calls", calls)
	}
	summary, recent := sess.Export()
	if summary != "" || len(recent) != 2 {
		t.Fatalf("session mutated unexpectedly: summary=%q recent=%+v", summary, recent)
	}
}

func TestCompactFoldsOldestAtThreshold(t *testing.T) {
	sess := session.New("sys")
	for i := 0; i < 6; i++ { // 12 messages
		sess.AppendUser(fmt.Sprintf("u%d", i))
		sess.AppendAssistant(fmt.Sprintf("a%d", i))
	}
	p := &Pipeline{
		Analysis: []Candidate{{Model: "m", LLM: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
			return "rolled-up summary", nil
		}}}},
		MaxHistoryMessages: 4,
	}

	p.compact(sess)

	summary, recent := sess.Export()
	if summary != "rolled-up summary" {
		t.Fatalf("summary = %q", summary)
	}
	if len(recent) != 2 {
		t.Fatalf("expected 2 remaining messages, got %d: %+v", len(recent), recent)
	}
}

func TestCompactLeavesHistoryOnLLMError(t *testing.T) {
	sess := session.New("sys")
	for i := 0; i < 6; i++ {
		sess.AppendUser("u")
		sess.AppendAssistant("a")
	}
	_, before := sess.Export()

	p := &Pipeline{
		Analysis: []Candidate{{Model: "m", LLM: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
			return "", errors.New("llm down")
		}}}},
		MaxHistoryMessages: 4,
	}
	p.compact(sess)

	summary, after := sess.Export()
	if summary != "" {
		t.Fatalf("summary should stay empty on LLM error, got %q", summary)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("history should be untouched on LLM error:\nbefore=%+v\nafter=%+v", before, after)
	}
}

// ---- correct() -----------------------------------------------------------------

func TestAnalyzePassesEachStageForwardInOrder(t *testing.T) {
	var calls []string
	var analysisInput, judgeInput string
	p := &Pipeline{
		LLM: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
			calls = append(calls, "chat")
			return "chat draft", nil
		}},
		ChatModel: "chat",
		Analysis: []Candidate{{Model: "analysis", LLM: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
			calls = append(calls, "analysis")
			analysisInput = msgs[len(msgs)-1].Content
			return "analysis refinement", nil
		}}}},
		Judge: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
			calls = append(calls, "judge")
			judgeInput = msgs[len(msgs)-1].Content
			return "judge final", nil
		}},
		JudgeModel: "judge",
	}

	got, err := p.analyze(context.Background(), "task", "input", false)
	if err != nil || got != "judge final" {
		t.Fatalf("analyze() = (%q, %v), want Judge final", got, err)
	}
	if !reflect.DeepEqual(calls, []string{"chat", "analysis", "judge"}) {
		t.Fatalf("stage order = %v, want chat -> analysis -> judge", calls)
	}
	if !strings.Contains(analysisInput, "Chat draft to refine:\nchat draft") {
		t.Fatalf("Analysis input = %q, want Chat draft", analysisInput)
	}
	if !strings.Contains(judgeInput, "Chat draft:\nchat draft") ||
		!strings.Contains(judgeInput, "advisory refinement (analysis) ---\nanalysis refinement") {
		t.Fatalf("Judge input = %q, want Chat and Analysis outputs", judgeInput)
	}
}

func TestAnalyzeSingleCandidateIsAdvisoryToJudge(t *testing.T) {
	judgeCalls := 0
	var judgeMsgs []llm.Message
	p := &Pipeline{
		Analysis: []Candidate{{Model: "solo", LLM: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
			return "solo answer", nil
		}}}},
		Judge: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
			judgeCalls++
			judgeMsgs = msgs
			return "judge's independent answer", nil
		}},
		JudgeModel: "judge",
	}
	got, err := p.analyze(context.Background(), "sys", "input", false)
	if err != nil {
		t.Fatalf("analyze() error = %v", err)
	}
	if got != "judge's independent answer" {
		t.Fatalf("analyze() = %q, want Judge's answer", got)
	}
	if judgeCalls != 1 {
		t.Fatalf("judge calls = %d, want 1 even for a single candidate", judgeCalls)
	}
	if len(judgeMsgs) != 2 {
		t.Fatalf("judge messages = %+v, want system and user messages", judgeMsgs)
	}
	if !strings.Contains(judgeMsgs[0].Content, "Analyze the ORIGINAL INPUT yourself") ||
		!strings.Contains(judgeMsgs[0].Content, "ORIGINAL TASK (authoritative):\nsys") {
		t.Fatalf("judge system prompt should require an independent analysis under the original task, got %q", judgeMsgs[0].Content)
	}
	if !strings.Contains(judgeMsgs[1].Content, "Original input:\ninput") ||
		!strings.Contains(judgeMsgs[1].Content, "--- advisory refinement (solo) ---\nsolo answer") {
		t.Fatalf("judge user input should contain the original input and advisory result, got %q", judgeMsgs[1].Content)
	}
}

func TestAnalyzeMultipleCandidatesAreAdvisoryToJudge(t *testing.T) {
	var judgeInput string
	p := &Pipeline{
		Analysis: []Candidate{
			{Model: "gemma-4-e4b", LLM: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
				return "candidate A", nil
			}}},
			{Model: "qwen3-6-35b-a3b", LLM: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
				return "candidate B", nil
			}}},
		},
		Judge: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
			judgeInput = msgs[len(msgs)-1].Content
			return "judge answer", nil
		}},
		JudgeModel: "judge-model",
	}
	got, err := p.analyze(context.Background(), "sys", "input", false)
	if err != nil {
		t.Fatalf("analyze() error = %v", err)
	}
	if got != "judge answer" {
		t.Fatalf("analyze() = %q, want %q", got, "judge answer")
	}
	if !strings.Contains(judgeInput, "candidate A") || !strings.Contains(judgeInput, "candidate B") {
		t.Fatalf("judge input should include both candidates' outputs, got %q", judgeInput)
	}
}

func TestAnalyzeFallsBackToFirstCandidateOnJudgeError(t *testing.T) {
	p := &Pipeline{
		Analysis: []Candidate{
			{Model: "first", LLM: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
				return "first candidate answer", nil
			}}},
			{Model: "second", LLM: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
				return "second candidate answer", nil
			}}},
		},
		Judge: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
			return "", errors.New("judge down")
		}},
		JudgeModel: "judge-model",
	}
	got, err := p.analyze(context.Background(), "sys", "input", false)
	if err != nil {
		t.Fatalf("analyze() error = %v", err)
	}
	if got != "first candidate answer" {
		t.Fatalf("analyze() = %q, want the first configured candidate's answer", got)
	}
}

func TestAnalyzeCallsJudgeWithSingleSurvivingCandidate(t *testing.T) {
	judgeCalls := 0
	var judgeInput string
	p := &Pipeline{
		Analysis: []Candidate{
			{Model: "flaky", LLM: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
				return "", errors.New("down")
			}}},
			{Model: "ok", LLM: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
				return "only surviving answer", nil
			}}},
		},
		Judge: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
			judgeCalls++
			judgeInput = msgs[len(msgs)-1].Content
			return "judge answer", nil
		}},
		JudgeModel: "judge-model",
	}
	got, err := p.analyze(context.Background(), "sys", "input", false)
	if err != nil {
		t.Fatalf("analyze() error = %v", err)
	}
	if got != "judge answer" {
		t.Fatalf("analyze() = %q, want Judge's answer", got)
	}
	if judgeCalls != 1 {
		t.Fatalf("judge calls = %d, want 1 when one candidate survives", judgeCalls)
	}
	if !strings.Contains(judgeInput, "only surviving answer") || strings.Contains(judgeInput, "flaky") {
		t.Fatalf("judge should receive only successful advisory results, got %q", judgeInput)
	}
}

func TestAnalyzeAllCandidatesFailWithoutJudgeReturnsError(t *testing.T) {
	p := &Pipeline{
		Analysis: []Candidate{
			{Model: "a", LLM: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
				return "", errors.New("down a")
			}}},
			{Model: "b", LLM: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
				return "", errors.New("down b")
			}}},
		},
	}
	if _, err := p.analyze(context.Background(), "sys", "input", false); err == nil {
		t.Fatal("expected an error when every candidate fails")
	}
}

func TestAnalyzeJudgeWorksWhenAllCandidatesFail(t *testing.T) {
	var judgeInput string
	p := &Pipeline{
		Analysis: []Candidate{
			{Model: "a", LLM: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
				return "", errors.New("down a")
			}}},
			{Model: "b", LLM: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
				return "", errors.New("down b")
			}}},
		},
		Judge: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
			judgeInput = msgs[len(msgs)-1].Content
			return "independent answer", nil
		}},
		JudgeModel: "judge",
	}

	got, err := p.analyze(context.Background(), "sys", "input", false)
	if err != nil {
		t.Fatalf("analyze() error = %v", err)
	}
	if got != "independent answer" {
		t.Fatalf("analyze() = %q, want Judge's independent answer", got)
	}
	if !strings.Contains(judgeInput, "(none available; verify and finalize the Chat draft yourself)") {
		t.Fatalf("judge input should explicitly handle unavailable advisory analyses, got %q", judgeInput)
	}
}

func TestAnalyzeJudgeWorksWithoutAnalysisCandidates(t *testing.T) {
	p := &Pipeline{
		Judge: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
			return "judge-only answer", nil
		}},
		JudgeModel: "judge",
	}
	got, err := p.analyze(context.Background(), "sys", "input", false)
	if err != nil {
		t.Fatalf("analyze() error = %v", err)
	}
	if got != "judge-only answer" {
		t.Fatalf("analyze() = %q, want %q", got, "judge-only answer")
	}
}

func TestAnalyzeNoModelsConfiguredReturnsError(t *testing.T) {
	p := &Pipeline{}
	if _, err := p.analyze(context.Background(), "sys", "input", false); err == nil {
		t.Fatal("expected an error when neither Analysis nor Judge is configured")
	}
}

// ---- transcribe() -----------------------------------------------------------
