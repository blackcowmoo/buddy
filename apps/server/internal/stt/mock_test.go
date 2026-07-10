package stt

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestMockTranscribeReturnsLabelAndWaits(t *testing.T) {
	m := NewMock("fast", 10*time.Millisecond)
	start := time.Now()
	res, err := m.Transcribe(context.Background(), make([]byte, 32000)) // 1s of 16kHz mono 16-bit audio
	if err != nil {
		t.Fatalf("Transcribe() error = %v", err)
	}
	if elapsed := time.Since(start); elapsed < 10*time.Millisecond {
		t.Fatalf("Transcribe() returned before its configured delay: %v", elapsed)
	}
	if !strings.Contains(res.Text, "mock-fast") {
		t.Fatalf("Text = %q, want it to mention the label", res.Text)
	}
	if !strings.Contains(res.Text, "1.0s") {
		t.Fatalf("Text = %q, want it to report ~1.0s of audio", res.Text)
	}
	if m.Name() != "mock(fast)" {
		t.Fatalf("Name() = %q, want mock(fast)", m.Name())
	}
}

func TestMockTranscribeRespectsCancellation(t *testing.T) {
	m := NewMock("slow", time.Hour) // long enough that only cancellation can end it
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := m.Transcribe(ctx, nil); err == nil {
		t.Fatal("expected an error when the context is already cancelled")
	}
}
