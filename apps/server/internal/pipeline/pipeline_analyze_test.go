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

func TestAnalyzeSingleCandidateSkipsJudge(t *testing.T) {
	judgeCalls := 0
	p := &Pipeline{
		Analysis: []Candidate{{Model: "solo", LLM: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
			return "solo answer", nil
		}}}},
		Judge: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
			judgeCalls++
			return "should not be called", nil
		}},
		JudgeModel: "judge",
	}
	got, err := p.analyze(context.Background(), "sys", "input", false)
	if err != nil {
		t.Fatalf("analyze() error = %v", err)
	}
	if got != "solo answer" {
		t.Fatalf("analyze() = %q, want %q", got, "solo answer")
	}
	if judgeCalls != 0 {
		t.Fatalf("judge should not be called for a single candidate, got %d calls", judgeCalls)
	}
}

func TestAnalyzeMultipleCandidatesSynthesizedByJudge(t *testing.T) {
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
			return "synthesized answer", nil
		}},
		JudgeModel: "judge-model",
	}
	got, err := p.analyze(context.Background(), "sys", "input", false)
	if err != nil {
		t.Fatalf("analyze() error = %v", err)
	}
	if got != "synthesized answer" {
		t.Fatalf("analyze() = %q, want %q", got, "synthesized answer")
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

func TestAnalyzeSkipsFailedCandidateWithoutCallingJudge(t *testing.T) {
	judgeCalls := 0
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
			return "unused", nil
		}},
		JudgeModel: "judge-model",
	}
	got, err := p.analyze(context.Background(), "sys", "input", false)
	if err != nil {
		t.Fatalf("analyze() error = %v", err)
	}
	if got != "only surviving answer" {
		t.Fatalf("analyze() = %q, want %q", got, "only surviving answer")
	}
	if judgeCalls != 0 {
		t.Fatalf("judge should not be called when only one candidate survives, got %d calls", judgeCalls)
	}
}

func TestAnalyzeAllCandidatesFailReturnsError(t *testing.T) {
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

func TestAnalyzeNoCandidatesConfiguredReturnsError(t *testing.T) {
	p := &Pipeline{}
	if _, err := p.analyze(context.Background(), "sys", "input", false); err == nil {
		t.Fatal("expected an error when no candidates are configured")
	}
}

// ---- transcribe() -----------------------------------------------------------
