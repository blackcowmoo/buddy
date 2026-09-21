package pipeline

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"buddy/server/internal/llm"
	"buddy/server/internal/workguard"
)

func TestDeletionStopsCascadeAndDiscardsFallback(t *testing.T) {
	for _, stage := range []string{"chat", "analysis", "judge"} {
		t.Run(stage, func(t *testing.T) {
			var deleted atomic.Bool
			calls := []string{}
			model := func(name string) llm.Client {
				return &fakeLLM{complete: func([]llm.Message) (string, error) {
					calls = append(calls, name)
					if stage == name {
						deleted.Store(true)
					}
					return "successful but possibly deleted response", nil
				}}
			}
			p := &Pipeline{LLM: model("chat"), ChatModel: "chat", Analysis: []Candidate{{LLM: model("analysis"), Model: "analysis"}}, Judge: model("judge"), JudgeModel: "judge"}
			ctx := workguard.Bind(context.Background(), func(context.Context) error {
				if deleted.Load() {
					return workguard.ErrDeleted
				}
				return nil
			})
			result, err := p.analyze(ctx, "task", "input", false)
			if !errors.Is(err, workguard.ErrDeleted) || result != "" {
				t.Fatalf("result=%q error=%v", result, err)
			}
			want := map[string]int{"chat": 1, "analysis": 2, "judge": 3}[stage]
			if len(calls) != want {
				t.Fatalf("calls=%v, want %d", calls, want)
			}
		})
	}
}
