package stt

import (
	"context"
	"fmt"
	"time"
)

// Mock is a zero-dependency Recognizer so the whole pipeline runs before you
// install any models. It reports how much audio it "heard" and pretends to
// think for a moment.
type Mock struct {
	Label string // shown in transcripts, e.g. "fast" / "slow"
	Delay time.Duration
}

func NewMock(label string, delay time.Duration) *Mock {
	return &Mock{Label: label, Delay: delay}
}

func (m *Mock) Name() string { return "mock(" + m.Label + ")" }

func (m *Mock) Transcribe(ctx context.Context, pcm []byte) (Result, error) {
	select {
	case <-time.After(m.Delay):
	case <-ctx.Done():
		return Result{}, ctx.Err()
	}
	// 16-bit mono @ 16kHz => 32000 bytes/sec.
	secs := float64(len(pcm)) / 32000.0
	return Result{
		Text:       fmt.Sprintf("[mock-%s heard ~%.1fs of audio] hello, how are you today?", m.Label, secs),
		Confidence: 0.5,
	}, nil
}
