package pipeline

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"

	"buddy/server/internal/checkpoint"
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
	key := modelCallKey(client, model)
	return workguard.Run(ctx, func(ctx context.Context) error {
		return p.modelCallQueue().Do(ctx, key, func() error {
			if err := workguard.Check(ctx); err != nil {
				return err
			}
			return call(ctx)
		})
	})
}

func modelCallKey(client llm.Client, model string) string {
	if keyed, ok := client.(llm.QueueKeyer); ok {
		if clientKey := keyed.QueueKey(model); clientKey != "" {
			return clientKey
		}
	}
	return model
}

func modelCheckpointKey(client llm.Client, model string, msgs []llm.Message, jsonMode, stream bool) string {
	// Structured encoding preserves message boundaries even when prompts contain
	// separators. Endpoint, model, and request changes invalidate prior results.
	input, _ := json.Marshal(struct {
		Endpoint string        `json:"endpoint"`
		Model    string        `json:"model"`
		Messages []llm.Message `json:"messages"`
		JSON     bool          `json:"json"`
		Stream   bool          `json:"stream"`
	}{modelCallKey(client, model), model, msgs, jsonMode, stream})
	return fmt.Sprintf("llm:v1:%x", sha256.Sum256(input))
}

func (p *Pipeline) complete(ctx context.Context, client llm.Client, model string, msgs []llm.Message, jsonMode bool, reusable ...func(string) bool) (string, error) {
	var accepts func(string) bool
	if len(reusable) > 0 {
		accepts = reusable[0]
	}
	// Check before entering the GPU queue: a completed step needs no model slot.
	return checkpoint.DoIf(ctx, modelCheckpointKey(client, model, msgs, jsonMode, false), func() (text string, err error) {
		err = p.runModelCall(ctx, client, model, func(ctx context.Context) error {
			text, err = client.Complete(ctx, model, msgs, jsonMode)
			return err
		})
		return text, err
	}, accepts)
}

func (p *Pipeline) chatStream(ctx context.Context, client llm.Client, model string, msgs []llm.Message, onToken func(string)) (string, error) {
	generated := false
	text, err := checkpoint.Do(ctx, modelCheckpointKey(client, model, msgs, false, true), func() (text string, err error) {
		generated = true
		err = p.runModelCall(ctx, client, model, func(ctx context.Context) error {
			text, err = client.ChatStream(ctx, model, msgs, func(token string) {
				if ctx.Err() == nil && onToken != nil {
					onToken(token)
				}
			})
			return err
		})
		return text, err
	})
	if err == nil && !generated && onToken != nil {
		if err := workguard.Check(ctx); err != nil {
			return "", err
		}
		onToken(text)
		if err := workguard.Check(ctx); err != nil {
			return "", err
		}
	}
	return text, err
}
