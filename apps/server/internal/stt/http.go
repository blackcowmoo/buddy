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

	"buddy/server/internal/wav"
)

// HTTPTranscriber talks to whisper.cpp's `server` example's native
// `/inference` endpoint — parakeet.cpp or similar can be swapped in as long
// as they speak the same multipart contract, no pipeline changes needed.
//
//	whisper-server -m ggml-large-v3-turbo.bin --port 8082
//
// Multiple URLs round-robin across replicas, so one slow request can't queue
// behind another on a single instance. Models is paired by index with URLs
// (config.parseModelURLPairs parses "model@url" entries from one env var) so
// a fleet of replicas running different models is expressible without a
// second list to keep in sync — an empty slot just omits the "model" field
// for that endpoint.
type HTTPTranscriber struct {
	Engine string   // label for Name(), e.g. "whisper", "parakeet"
	URLs   []string // one or more server roots
	Models []string // paired by index with URLs; "" omits the "model" field

	http *http.Client
	next uint64
	name string // built once in NewHTTPTranscriber; Transcribe calls Name() on every utterance
}

func NewHTTPTranscriber(engine string, urls, models []string) *HTTPTranscriber {
	return &HTTPTranscriber{
		Engine: engine,
		URLs:   urls,
		Models: models,
		http:   &http.Client{Timeout: 60 * time.Second},
		name:   engine + "-server(" + strings.Join(uniqueNonEmpty(models), ",") + ")",
	}
}

func (h *HTTPTranscriber) Name() string {
	return h.name
}

// pickEndpoint round-robins across URLs (so concurrent fast/refine-track
// calls spread across replicas instead of piling onto the first one) and
// returns the model paired with whichever URL was picked.
func (h *HTTPTranscriber) pickEndpoint() (url, model string) {
	i := atomic.AddUint64(&h.next, 1) - 1
	idx := i % uint64(len(h.URLs))
	url = h.URLs[idx]
	if idx < uint64(len(h.Models)) {
		model = h.Models[idx]
	}
	return url, model
}

func (h *HTTPTranscriber) Transcribe(ctx context.Context, pcm []byte) (Result, error) {
	if len(h.URLs) == 0 {
		return Result{}, fmt.Errorf("%s: no server URLs configured", h.Engine)
	}
	url, model := h.pickEndpoint()

	var wavBuf bytes.Buffer
	if err := wav.Encode(&wavBuf, pcm, 16000, 1); err != nil {
		return Result{}, err
	}

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, err := mw.CreateFormFile("file", "utterance.wav")
	if err != nil {
		return Result{}, err
	}
	if _, err := io.Copy(fw, &wavBuf); err != nil {
		return Result{}, err
	}
	if model != "" {
		_ = mw.WriteField("model", model)
	}
	_ = mw.WriteField("response_format", "json")
	if err := mw.Close(); err != nil {
		return Result{}, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(url, "/")+"/inference", &body)
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

func uniqueNonEmpty(ss []string) []string {
	seen := make(map[string]bool, len(ss))
	var out []string
	for _, s := range ss {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}
