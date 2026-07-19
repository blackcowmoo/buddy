package recording

import (
	"bytes"

	"buddy/server/internal/wav"
)

const (
	bitsPerSample  = 16
	channels       = 1 // mono, matching the wire format in internal/protocol
	bytesPerSample = bitsPerSample / 8
)

// encodeWAV wraps raw mono 16-bit little-endian PCM (the wire format
// documented in internal/protocol) in a canonical 44-byte WAV header, so the
// stored file plays directly in a browser <audio> element with no further
// decoding on our side.
func encodeWAV(pcm []byte, sampleRate int) []byte {
	var out bytes.Buffer
	out.Grow(44 + len(pcm))
	_ = wav.Encode(&out, pcm, sampleRate, channels) // bytes.Buffer.Write never errors
	return out.Bytes()
}

// durationMS returns the playback length of raw mono 16-bit PCM at
// sampleRate, in milliseconds.
func durationMS(pcm []byte, sampleRate int) int {
	if sampleRate <= 0 {
		return 0
	}
	samples := len(pcm) / bytesPerSample
	return samples * 1000 / sampleRate
}
