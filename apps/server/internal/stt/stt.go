// Package stt defines the speech-to-text seam. Everything that turns audio
// into text implements Recognizer, so the "legacy/fast" and "LLM/slow"
// engines are swappable behind the same interface.
package stt

import "context"

// Result is a single transcription.
type Result struct {
	Text       string
	Confidence float64 // 0..1, best-effort (engines may not provide it)
}

// Recognizer transcribes one complete utterance.
//
// pcm is mono, 16 kHz, signed 16-bit little-endian PCM — the lingua franca
// for whisper/vosk. The browser resamples to this before sending.
type Recognizer interface {
	Name() string
	Transcribe(ctx context.Context, pcm []byte) (Result, error)
}

// StreamingRecognizer is the upgrade path for true partial results
// (e.g. Vosk). Not required by the current pipeline — implement it when you
// wire a Kaldi/Vosk engine and want token-by-token partials.
type StreamingRecognizer interface {
	Recognizer
	// Stream consumes audio chunks from `in` and emits partial/final results
	// on the returned channel until `in` is closed.
	Stream(ctx context.Context, in <-chan []byte) (<-chan Result, error)
}
