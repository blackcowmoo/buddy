// Package llm is the seam for the chat + correction models. The default
// implementation targets any OpenAI-compatible chat-completions server
// (llama.cpp's llama-server, vLLM, LM Studio, or the OpenAI API), so the engine
// swaps by config alone without touching the pipeline.
package llm

import "context"

const (
	RoleSystem    = "system"
	RoleUser      = "user"
	RoleAssistant = "assistant"
)

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Client is the minimal surface the pipeline needs.
type Client interface {
	// ChatStream streams the assistant reply token-by-token via onToken and
	// returns the full concatenated text.
	ChatStream(ctx context.Context, model string, msgs []Message, onToken func(string)) (string, error)
	// Complete returns a single non-streamed completion (used for the
	// background grammar/context correction pass).
	Complete(ctx context.Context, model string, msgs []Message, jsonMode bool) (string, error)
}

// QueueKeyer lets a client distinguish two models served by different
// endpoints even when their model names are identical. Clients that do not
// implement it are queued by the model name supplied to Client methods.
type QueueKeyer interface {
	QueueKey(model string) string
}
