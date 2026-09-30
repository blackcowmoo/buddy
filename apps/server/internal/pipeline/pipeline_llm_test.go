package pipeline

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"buddy/server/internal/llm"
)

func TestBackgroundCascadeYieldsAtEveryModel(t *testing.T) {
	for _, stage := range []string{"chat", "analysis", "judge"} {
		for _, stream := range []bool{false, true} {
			name := stage + "/complete"
			if stream {
				name = stage + "/stream"
			}
			t.Run(name, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					var calls []string
					model := func(name string) *fakeLLM {
						return &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
							calls = append(calls, name)
							return "migration result", nil
						}}
					}
					p := &Pipeline{
						LLM: model("chat"), ChatModel: "chat",
						Analysis: []Candidate{{LLM: model("analysis"), Model: "analysis"}},
						Judge:    model("judge"), JudgeModel: "judge",
					}
					release := make(chan struct{})
					unblock := sync.OnceFunc(func() { close(release) })
					defer unblock()
					go func() {
						if err := p.modelCallQueue().Do(context.Background(), stage, func() error {
							<-release
							return nil
						}); err != nil {
							t.Errorf("occupy %s: %v", stage, err)
						}
					}()
					synctest.Wait()
					go func() {
						got, err := p.analyze(llm.WithBackgroundPriority(context.Background()), "migration", "old entry", false)
						if err != nil || got != "migration result" {
							t.Errorf("migration = %q, %v", got, err)
						}
					}()
					synctest.Wait()
					interactive := &fakeLLM{
						complete: func([]llm.Message) (string, error) {
							calls = append(calls, "interactive")
							return "answer", nil
						},
						chatReply: "answer",
						onChat:    func([]llm.Message) { calls = append(calls, "interactive") },
					}
					go func() {
						var got string
						var err error
						if stream {
							got, err = p.chatStream(context.Background(), interactive, stage, nil, nil)
						} else {
							got, err = p.complete(context.Background(), interactive, stage, nil, false)
						}
						if err != nil || got != "answer" {
							t.Errorf("interactive = %q, %v", got, err)
						}
					}()
					synctest.Wait()
					unblock()
					synctest.Wait()
					want := []string{"chat", "analysis", "judge"}
					want = slices.Insert(want, slices.Index(want, stage), "interactive")
					if !slices.Equal(calls, want) {
						t.Errorf("model calls = %v, want %v", calls, want)
					}
				})
			})
		}
	}
}

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
