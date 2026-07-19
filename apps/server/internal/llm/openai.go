package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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
		http:    &http.Client{Timeout: 120 * time.Second},
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

// do posts body to the chat-completions endpoint and returns the response
// once its status has checked out OK — callers only need to decode the body
// (streamed SSE or a single JSON payload) and close it when done.
func (o *OpenAI) do(ctx context.Context, body []byte) (*http.Response, error) {
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
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("llm status %d", resp.StatusCode)
	}
	return resp, nil
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
