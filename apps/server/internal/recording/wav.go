package recording

import (
	"encoding/binary"
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
	byteRate := sampleRate * channels * bytesPerSample
	blockAlign := channels * bytesPerSample
	dataLen := uint32(len(pcm))

	out := make([]byte, 44+len(pcm))
	copy(out[0:4], "RIFF")
	binary.LittleEndian.PutUint32(out[4:8], 36+dataLen)
	copy(out[8:12], "WAVE")
	copy(out[12:16], "fmt ")
	binary.LittleEndian.PutUint32(out[16:20], 16) // fmt chunk size
	binary.LittleEndian.PutUint16(out[20:22], 1)  // PCM
	binary.LittleEndian.PutUint16(out[22:24], uint16(channels))
	binary.LittleEndian.PutUint32(out[24:28], uint32(sampleRate))
	binary.LittleEndian.PutUint32(out[28:32], uint32(byteRate))
	binary.LittleEndian.PutUint16(out[32:34], uint16(blockAlign))
	binary.LittleEndian.PutUint16(out[34:36], bitsPerSample)
	copy(out[36:40], "data")
	binary.LittleEndian.PutUint32(out[40:44], dataLen)
	copy(out[44:], pcm)
	return out
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
