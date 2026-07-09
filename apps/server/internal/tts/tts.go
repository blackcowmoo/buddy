// Package tts is an extension point. In this project the primary TTS is
// kokoro-82M running in the browser (WebGPU), so the backend does NOT
// synthesize audio in the default flow.
//
// Keep this seam for later: server-side TTS is useful for non-browser
// clients, phone/SIP gateways, or pre-rendering. A good no-Python
// implementation is piper (https://github.com/rhasspy/piper) shelled out the
// same way stt.Whisper is, or kokoro-onnx via onnxruntime's Go bindings.
package tts

import "context"

type Audio struct {
	Data []byte
	MIME string // e.g. "audio/wav"
}

type Synthesizer interface {
	Name() string
	Synthesize(ctx context.Context, text, voice string) (Audio, error)
}

// Noop is the default: synthesis happens client-side.
type Noop struct{}

func (Noop) Name() string { return "noop(browser-kokoro)" }
func (Noop) Synthesize(ctx context.Context, text, voice string) (Audio, error) {
	return Audio{}, nil
}
