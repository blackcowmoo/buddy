package stt

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

// HTTPTranscriber talks to any OpenAI-compatible /v1/audio/transcriptions
// server — whisper.cpp's `server` example, parakeet.cpp, or similar. Point
// URLs at a different server/engine to swap it, no pipeline changes needed.
//
//	whisper-server -m ggml-large-v3-turbo.bin --port 8082   # OpenAI-compatible
//
// Multiple URLs round-robin across replicas of the same engine, so one slow
// request can't queue behind another on a single instance.
type HTTPTranscriber struct {
	Engine string   // label for Name(), e.g. "whisper", "parakeet"
	URLs   []string // one or more "/v1" roots
	Model  string   // sent as the "model" form field; omitted if empty

	http *http.Client
	next uint64
}

func NewHTTPTranscriber(engine string, urls []string, model string) *HTTPTranscriber {
	return &HTTPTranscriber{
		Engine: engine,
		URLs:   urls,
		Model:  model,
		http:   &http.Client{Timeout: 60 * time.Second},
	}
}

func (h *HTTPTranscriber) Name() string { return h.Engine + "-server(" + h.Model + ")" }

// pickURL round-robins across URLs so concurrent fast/refine-track calls
// spread across replicas instead of piling onto the first one.
func (h *HTTPTranscriber) pickURL() string {
	i := atomic.AddUint64(&h.next, 1) - 1
	return h.URLs[i%uint64(len(h.URLs))]
}

func (h *HTTPTranscriber) Transcribe(ctx context.Context, pcm []byte) (Result, error) {
	if len(h.URLs) == 0 {
		return Result{}, fmt.Errorf("%s: no server URLs configured", h.Engine)
	}

	var wav bytes.Buffer
	if err := writeWAV(&wav, pcm, 16000, 1); err != nil {
		return Result{}, err
	}

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, err := mw.CreateFormFile("file", "utterance.wav")
	if err != nil {
		return Result{}, err
	}
	if _, err := io.Copy(fw, &wav); err != nil {
		return Result{}, err
	}
	if h.Model != "" {
		_ = mw.WriteField("model", h.Model)
	}
	if err := mw.Close(); err != nil {
		return Result{}, err
	}

	url := strings.TrimRight(h.pickURL(), "/") + "/audio/transcriptions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, &body)
	if err != nil {
		return Result{}, err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())

	resp, err := h.http.Do(req)
	if err != nil {
		return Result{}, fmt.Errorf("%s: unreachable: %w", h.Engine, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return Result{}, fmt.Errorf("%s: status %d: %s", h.Engine, resp.StatusCode, string(b))
	}

	var parsed struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return Result{}, err
	}
	return Result{Text: strings.TrimSpace(parsed.Text), Confidence: 0.9}, nil
}
