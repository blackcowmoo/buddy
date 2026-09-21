package pipeline

import (
	"context"
	"fmt"

	"buddy/server/internal/llm"
	"buddy/server/internal/workguard"
)

// runModelCall is the single concurrency boundary for LLM calls made by a
// Pipeline. The queue key is normally the model name; OpenAI-compatible
// clients add their endpoint through llm.QueueKeyer so two endpoints serving
// the same model do not unnecessarily block one another.
func (p *Pipeline) runModelCall(ctx context.Context, client llm.Client, model string, call func(context.Context) error) error {
	if client == nil {
		return fmt.Errorf("llm: no model configured")
	}
	key := model
	if keyed, ok := client.(llm.QueueKeyer); ok {
		if clientKey := keyed.QueueKey(model); clientKey != "" {
			key = clientKey
		}
	}
	return workguard.Run(ctx, func(ctx context.Context) error {
		return p.modelCallQueue().Do(ctx, key, func() error {
			if err := workguard.Check(ctx); err != nil {
				return err
			}
			return call(ctx)
		})
	})
}

func (p *Pipeline) complete(ctx context.Context, client llm.Client, model string, msgs []llm.Message, jsonMode bool) (text string, err error) {
	err = p.runModelCall(ctx, client, model, func(ctx context.Context) error {
		text, err = client.Complete(ctx, model, msgs, jsonMode)
		return err
	})
	return text, err
}

func (p *Pipeline) chatStream(ctx context.Context, client llm.Client, model string, msgs []llm.Message, onToken func(string)) (text string, err error) {
	err = p.runModelCall(ctx, client, model, func(ctx context.Context) error {
		text, err = client.ChatStream(ctx, model, msgs, func(token string) {
			if ctx.Err() == nil {
				onToken(token)
			}
		})
		return err
	})
	return text, err
}
