package pipeline

import (
	"context"

	"buddy/server/internal/llm"
)

// analyzeJSON uses one decoder for checkpoint validation and the returned
// artifact, so a result rejected by the caller remains retryable at every stage.
func analyzeJSON[T any](ctx context.Context, p *Pipeline, systemPrompt, input string, decode func(string) (T, error)) (T, error) {
	raw, err := p.analyze(ctx, systemPrompt, input, true, reusableOutput(decode))
	if err != nil {
		var zero T
		return zero, err
	}
	return decode(raw)
}

// chatJSON applies the same validation to a single fast Chat call, without
// invoking Analysis or Judge for latency-sensitive vocabulary lookups.
func chatJSON[T any](ctx context.Context, p *Pipeline, systemPrompt, input string, decode func(string) (T, error)) (T, error) {
	raw, err := p.complete(ctx, p.LLM, p.ChatModel, []llm.Message{
		{Role: llm.RoleSystem, Content: systemPrompt},
		{Role: llm.RoleUser, Content: input},
	}, true, reusableOutput(decode))
	if err != nil {
		var zero T
		return zero, err
	}
	return decode(raw)
}

// reusableOutput applies the same decoder used to publish a result before it
// can become a durable checkpoint. A successful model request may still carry
// an invalid artifact that a retry needs to regenerate.
func reusableOutput[T any](decode func(string) (T, error)) func(string) bool {
	return func(raw string) bool {
		_, err := decode(raw)
		return err == nil
	}
}
