// Package stt defines the speech-to-text seam. Everything that turns audio
// into text implements Recognizer, so the "legacy/fast" and "LLM/slow"
// engines are swappable behind the same interface.
package stt

import "context"

// Result is a single transcription.
type Result struct {
	Text string
}

// Recognizer transcribes one complete utterance.
//
// pcm is mono, 16 kHz, signed 16-bit little-endian PCM — the lingua franca
// for whisper/vosk. The browser resamples to this before sending.
type Recognizer interface {
	Name() string
	Transcribe(ctx context.Context, pcm []byte) (Result, error)
}
