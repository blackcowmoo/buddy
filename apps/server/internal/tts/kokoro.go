// Package tts talks to an OpenAI-compatible /v1/audio/speech server —
// Kokoro-FastAPI (https://github.com/remsky/Kokoro-FastAPI) is the one this
// app targets, chosen for the same reason the browser-side "오늘의 아티클"
// read-aloud used kokoro-82M directly (see apps/web/src/tts/kokoro.ts): best
// quality-per-CPU-cycle among self-hosted open TTS models, no GPU required.
// Moving generation server-side (see internal/transport's article study job
// and the chat message audio handler) removes the reliability problems that
// pipeline had running the same model inside mobile Safari — WebGPU
// flakiness, WASM slowness, and a HuggingFace fetch on the critical path —
// by generating once, server-side, and caching the result (see
// internal/ttsstore) instead of regenerating in every learner's browser.
package tts

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Speaker is the minimal surface callers like internal/transport.ArticleAudio
// need — Speak alone — so they (and their own tests) can depend on this
// interface instead of the concrete *Kokoro client, the same "swap for a
// fake in tests, real client in production" shape as internal/llm.Client.
type Speaker interface {
	Speak(ctx context.Context, text string) ([]byte, error)
}

// Kokoro is an HTTP client for one /v1/audio/speech endpoint.
type Kokoro struct {
	BaseURL string // the /v1 root, e.g. http://kokoro:8880/v1
	Voice   string // e.g. "af_heart" — see kokoro.ts's KokoroVoice for why this app only ever uses one
	APIKey  string // optional; sent as "Authorization: Bearer <key>"
	http    *http.Client
}

// requestTimeout bounds one generation call. CPU-only synthesis of a long
// passage can legitimately take tens of seconds (see kokoro.ts's
// GENERATION_TIMEOUT_MS for the client-side equivalent reasoning), but
// article/message text here is bounded to a paragraph or so, not arbitrary
// length, so a generous fixed ceiling — rather than no timeout at all — is
// enough to turn a genuinely stuck upstream into a caught error instead of
// an indefinitely hung request.
const requestTimeout = 2 * time.Minute

// NewKokoro normalizes baseURL to end in exactly one "/v1", so a config
// value with or without the suffix both reach POST {baseURL}/audio/speech.
// An empty voice defaults to "af_heart", the one voice this app's UI has
// ever exposed (see kokoro.ts).
func NewKokoro(baseURL, voice, apiKey string) *Kokoro {
	baseURL = strings.TrimRight(baseURL, "/")
	if !strings.HasSuffix(baseURL, "/v1") {
		baseURL += "/v1"
	}
	if voice == "" {
		voice = "af_heart"
	}
	return &Kokoro{
		BaseURL: baseURL,
		Voice:   voice,
		APIKey:  apiKey,
		http:    &http.Client{Timeout: requestTimeout},
	}
}

type speechReq struct {
	Model          string `json:"model"`
	Input          string `json:"input"`
	Voice          string `json:"voice"`
	ResponseFormat string `json:"response_format"`
	Stream         bool   `json:"stream,omitempty"`
}

// responseFormat is mp3 everywhere: far smaller than wav for the same
// content (a 275-char paragraph is ~840KB as wav vs. a fraction of that as
// mp3), which matters directly for S3 storage cost — the whole reason a
// generated-audio TTL exists at all (see internal/ttsstore).
const responseFormat = "mp3"

// Speak synthesizes text and returns the complete mp3 bytes, already
// buffered. For call sites that need the full byte count up front anyway
// (S3 upload — see internal/ttsstore.Store.Put) rather than relaying bytes
// as they arrive; see Stream for the latter.
func (k *Kokoro) Speak(ctx context.Context, text string) ([]byte, error) {
	resp, err := k.do(ctx, text, false)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

// Stream synthesizes text and returns the response body unbuffered, for
// callers relaying bytes to a client as they arrive instead of waiting for
// the whole thing (an on-demand chat message read-aloud, where
// time-to-first-byte is what "responds immediately" actually depends on).
// The caller must Close the returned body.
func (k *Kokoro) Stream(ctx context.Context, text string) (io.ReadCloser, error) {
	resp, err := k.do(ctx, text, true)
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

func (k *Kokoro) do(ctx context.Context, text string, stream bool) (*http.Response, error) {
	body, err := json.Marshal(speechReq{
		Model:          "kokoro",
		Input:          text,
		Voice:          k.Voice,
		ResponseFormat: responseFormat,
		Stream:         stream,
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, k.BaseURL+"/audio/speech", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if k.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+k.APIKey)
	}
	resp, err := k.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("tts unreachable: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		return nil, fmt.Errorf("tts status %d: %s", resp.StatusCode, bytes.TrimSpace(respBody))
	}
	return resp, nil
}
