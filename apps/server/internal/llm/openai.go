package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// OpenAI talks to any OpenAI-compatible chat-completions endpoint over HTTP.
// This covers llama.cpp's llama-server, vLLM, LM Studio, and the OpenAI API
// itself — point BaseURL at a different server to swap the engine, no pipeline
// changes needed.
//
//	llama-server -m model.gguf --port 8081   # serves /v1/chat/completions
type OpenAI struct {
	BaseURL string // the /v1 root, e.g. http://localhost:8081/v1
	APIKey  string // optional; sent as "Authorization: Bearer <key>"
	http    *http.Client
}

// requestTimeout bounds one chat-completions call end-to-end (including a
// streamed response's full duration, not just time-to-first-byte). Sized for
// a locally hosted model on modest hardware, which can legitimately take far
// longer than a hosted API to finish a single completion — not for detecting
// a hung request quickly. A short timeout here just turns a slow-but-healthy
// response into a failure, which callers then retry, piling more load onto
// an already-slow server (see asyncjob's matching claimTTL constants, which
// must stay comfortably above this).
const requestTimeout = 24 * time.Hour

// NewOpenAI normalizes baseURL to end in exactly one "/v1", so a config value
// with or without the suffix (e.g. a copy-pasted server address that forgot
// it) both reach POST {baseURL}/chat/completions instead of 404ing.
func NewOpenAI(baseURL, apiKey string) *OpenAI {
	baseURL = strings.TrimRight(baseURL, "/")
	if !strings.HasSuffix(baseURL, "/v1") {
		baseURL += "/v1"
	}
	return &OpenAI{
		BaseURL: baseURL,
		APIKey:  apiKey,
		http:    &http.Client{Timeout: requestTimeout},
	}
}

type chatReq struct {
	Model          string          `json:"model"`
	Messages       []Message       `json:"messages"`
	Stream         bool            `json:"stream"`
	ResponseFormat *responseFormat `json:"response_format,omitempty"`
}

type responseFormat struct {
	Type string `json:"type"` // "json_object"
}

// chatChunk is one streamed SSE delta ("stream": true).
type chatChunk struct {
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
		} `json:"delta"`
	} `json:"choices"`
}

// chatResp is a full non-streamed completion ("stream": false).
type chatResp struct {
	Choices []struct {
		Message Message `json:"message"`
	} `json:"choices"`
}

var (
	sseData = []byte("data:")
	sseDone = []byte("[DONE]")
)

// overflowResetReason identifies a 503 whose body is an Envoy-style
// connection-pool/circuit-breaker rejection ("upstream connect error or
// disconnect/reset before headers. reset reason: overflow") rather than a
// hard failure — the upstream LLM's gateway has no spare capacity for an
// instant, not an outage. Unlike other errors, this one is worth a few short
// retries before giving up to the caller's fallback (see overflowBackoff),
// since the same request tried again a moment later usually succeeds.
const overflowResetReason = "reset reason: overflow"

// overflowBackoff bounds how many times, and how long, do() waits between
// retries of an overflow 503 before surfacing it as a normal error. Kept
// short and finite so a genuinely stuck upstream still fails fast enough for
// the caller's fallback path (pipeline.fallbackReply) to kick in rather than
// holding the learner's turn open indefinitely.
var overflowBackoff = []time.Duration{500 * time.Millisecond, 1 * time.Second, 2 * time.Second}

// do posts body to the chat-completions endpoint and returns the response
// once its status has checked out OK — callers only need to decode the body
// (streamed SSE or a single JSON payload) and close it when done.
func (o *OpenAI) do(ctx context.Context, body []byte) (*http.Response, error) {
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.BaseURL+"/chat/completions", bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		if o.APIKey != "" {
			req.Header.Set("Authorization", "Bearer "+o.APIKey)
		}
		resp, err := o.http.Do(req)
		if err != nil {
			return nil, fmt.Errorf("llm unreachable: %w", err)
		}
		if resp.StatusCode == http.StatusOK {
			return resp, nil
		}
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		if resp.StatusCode == http.StatusServiceUnavailable && bytes.Contains(respBody, []byte(overflowResetReason)) && attempt < len(overflowBackoff) {
			select {
			case <-time.After(overflowBackoff[attempt]):
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return nil, fmt.Errorf("llm status %d: %s", resp.StatusCode, bytes.TrimSpace(respBody))
	}
}

func (o *OpenAI) ChatStream(ctx context.Context, model string, msgs []Message, onToken func(string)) (string, error) {
	body, _ := json.Marshal(chatReq{Model: model, Messages: msgs, Stream: true})
	resp, err := o.do(ctx, body)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var full bytes.Buffer
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		// SSE frames are "data: {json}" lines, terminated by "data: [DONE]".
		line := bytes.TrimSpace(sc.Bytes())
		if !bytes.HasPrefix(line, sseData) {
			continue
		}
		payload := bytes.TrimSpace(line[len(sseData):])
		if bytes.Equal(payload, sseDone) {
			break
		}
		var ch chatChunk
		if err := json.Unmarshal(payload, &ch); err != nil || len(ch.Choices) == 0 {
			continue
		}
		if tok := ch.Choices[0].Delta.Content; tok != "" {
			full.WriteString(tok)
			if onToken != nil {
				onToken(tok)
			}
		}
	}
	return full.String(), sc.Err()
}

func (o *OpenAI) Complete(ctx context.Context, model string, msgs []Message, jsonMode bool) (string, error) {
	r := chatReq{Model: model, Messages: msgs, Stream: false}
	if jsonMode {
		r.ResponseFormat = &responseFormat{Type: "json_object"}
	}
	body, _ := json.Marshal(r)
	resp, err := o.do(ctx, body)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var cr chatResp
	if err := json.NewDecoder(resp.Body).Decode(&cr); err != nil {
		return "", err
	}
	if len(cr.Choices) == 0 {
		return "", fmt.Errorf("llm: empty choices")
	}
	return cr.Choices[0].Message.Content, nil
}
