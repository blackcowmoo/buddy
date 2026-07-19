// Package wav wraps raw mono 16-bit little-endian PCM (the wire format
// documented in internal/protocol) in a canonical 44-byte WAV header, so a
// stored or transcoded file plays/decodes with no further work on the
// consumer's side.
package wav

import (
	"encoding/binary"
	"io"
)

const bitsPerSample = 16

// Encode writes a 44-byte WAV header for pcm followed by pcm itself to w.
func Encode(w io.Writer, pcm []byte, sampleRate, channels int) error {
	byteRate := sampleRate * channels * bitsPerSample / 8
	blockAlign := channels * bitsPerSample / 8
	dataLen := uint32(len(pcm))

	h := make([]byte, 44)
	copy(h[0:4], "RIFF")
	binary.LittleEndian.PutUint32(h[4:8], 36+dataLen)
	copy(h[8:12], "WAVE")
	copy(h[12:16], "fmt ")
	binary.LittleEndian.PutUint32(h[16:20], 16) // fmt chunk size
	binary.LittleEndian.PutUint16(h[20:22], 1)  // PCM
	binary.LittleEndian.PutUint16(h[22:24], uint16(channels))
	binary.LittleEndian.PutUint32(h[24:28], uint32(sampleRate))
	binary.LittleEndian.PutUint32(h[28:32], uint32(byteRate))
	binary.LittleEndian.PutUint16(h[32:34], uint16(blockAlign))
	binary.LittleEndian.PutUint16(h[34:36], bitsPerSample)
	copy(h[36:40], "data")
	binary.LittleEndian.PutUint32(h[40:44], dataLen)

	if _, err := w.Write(h); err != nil {
		return err
	}
	_, err := w.Write(pcm)
	return err
}
