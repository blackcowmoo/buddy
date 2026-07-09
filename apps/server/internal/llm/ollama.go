package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// Ollama talks to a local Ollama daemon (https://ollama.com) over HTTP.
//
//	brew install ollama && ollama serve
//	ollama pull llama3.2:3b
type Ollama struct {
	BaseURL string
	http    *http.Client
}

func NewOllama(baseURL string) *Ollama {
	return &Ollama{
		BaseURL: baseURL,
		http:    &http.Client{Timeout: 120 * time.Second},
	}
}

type chatReq struct {
	Model    string         `json:"model"`
	Messages []Message      `json:"messages"`
	Stream   bool           `json:"stream"`
	Format   string         `json:"format,omitempty"`
	Options  map[string]any `json:"options,omitempty"`
}

type chatResp struct {
	Message Message `json:"message"`
	Done    bool    `json:"done"`
}

func (o *Ollama) ChatStream(ctx context.Context, model string, msgs []Message, onToken func(string)) (string, error) {
	body, _ := json.Marshal(chatReq{Model: model, Messages: msgs, Stream: true})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.BaseURL+"/api/chat", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := o.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("ollama unreachable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("ollama status %d", resp.StatusCode)
	}

	var full bytes.Buffer
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var cr chatResp
		if err := json.Unmarshal(line, &cr); err != nil {
			continue
		}
		if cr.Message.Content != "" {
			full.WriteString(cr.Message.Content)
			if onToken != nil {
				onToken(cr.Message.Content)
			}
		}
		if cr.Done {
			break
		}
	}
	return full.String(), sc.Err()
}

func (o *Ollama) Complete(ctx context.Context, model string, msgs []Message, jsonMode bool) (string, error) {
	r := chatReq{Model: model, Messages: msgs, Stream: false}
	if jsonMode {
		r.Format = "json"
	}
	body, _ := json.Marshal(r)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.BaseURL+"/api/chat", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := o.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("ollama unreachable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("ollama status %d", resp.StatusCode)
	}
	var cr chatResp
	if err := json.NewDecoder(resp.Body).Decode(&cr); err != nil {
		return "", err
	}
	return cr.Message.Content, nil
}
