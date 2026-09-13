package pipeline

import (
	"context"
	"strings"
	"testing"
	"time"

	"buddy/server/internal/llm"
)

// TestAnalyzeQueuesEachModelIndependently reproduces the important request
// interleaving: request A is holding llm2, request B must still enter llm1,
// and B's llm2 call must wait behind A's llm2 call. A request-wide semaphore
// would fail this test because B's llm1 would not be reached until A finished.
func TestAnalyzeQueuesEachModelIndependently(t *testing.T) {
	llm2StartedByA := make(chan struct{})
	releaseA := make(chan struct{})
	llm1StartedByB := make(chan struct{})
	llm2StartedByB := make(chan struct{})

	model1 := &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
		if strings.TrimSpace(msgs[len(msgs)-1].Content) == "request B" {
			close(llm1StartedByB)
		}
		return "model 1 result", nil
	}}
	model2 := &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
		switch strings.TrimSpace(msgs[len(msgs)-1].Content) {
		case "request A":
			close(llm2StartedByA)
			<-releaseA
		case "request B":
			close(llm2StartedByB)
		}
		return "model 2 result", nil
	}}

	p := &Pipeline{
		Analysis: []Candidate{
			{Model: "llm1", LLM: model1},
			{Model: "llm2", LLM: model2},
		},
	}

	aDone := make(chan error, 1)
	go func() {
		_, err := p.analyze(context.Background(), "task", "request A", false)
		aDone <- err
	}()
	waitForPipelineSignal(t, llm2StartedByA)

	bDone := make(chan error, 1)
	go func() {
		_, err := p.analyze(context.Background(), "task", "request B", false)
		bDone <- err
	}()
	waitForPipelineSignal(t, llm1StartedByB)

	select {
	case <-llm2StartedByB:
		t.Fatal("request B entered llm2 before request A released llm2")
	default:
	}

	close(releaseA)
	waitForPipelineSignal(t, llm2StartedByB)
	if err := <-aDone; err != nil {
		t.Fatalf("request A: %v", err)
	}
	if err := <-bDone; err != nil {
		t.Fatalf("request B: %v", err)
	}
}

func waitForPipelineSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for model call")
	}
}
