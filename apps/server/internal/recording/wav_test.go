package recording

import (
	"encoding/binary"
	"testing"
)

func TestEncodeWAVHeader(t *testing.T) {
	pcm := make([]byte, 32000) // 1 second of mono 16-bit @ 16kHz
	out := encodeWAV(pcm, 16000)

	if len(out) != 44+len(pcm) {
		t.Fatalf("len(out) = %d, want %d", len(out), 44+len(pcm))
	}
	if string(out[0:4]) != "RIFF" || string(out[8:12]) != "WAVE" {
		t.Fatalf("missing RIFF/WAVE markers: %q", out[0:12])
	}
	if string(out[12:16]) != "fmt " || string(out[36:40]) != "data" {
		t.Fatalf("missing fmt /data chunk markers")
	}
	if got := binary.LittleEndian.Uint16(out[20:22]); got != 1 {
		t.Errorf("audio format = %d, want 1 (PCM)", got)
	}
	if got := binary.LittleEndian.Uint16(out[22:24]); got != 1 {
		t.Errorf("channels = %d, want 1 (mono)", got)
	}
	if got := binary.LittleEndian.Uint32(out[24:28]); got != 16000 {
		t.Errorf("sample rate = %d, want 16000", got)
	}
	if got := binary.LittleEndian.Uint16(out[34:36]); got != 16 {
		t.Errorf("bits per sample = %d, want 16", got)
	}
	if got := binary.LittleEndian.Uint32(out[40:44]); got != uint32(len(pcm)) {
		t.Errorf("data chunk size = %d, want %d", got, len(pcm))
	}
	if string(out[44:]) != string(pcm) {
		t.Error("payload after the header does not match the input PCM")
	}
}

func TestDurationMS(t *testing.T) {
	tests := []struct {
		name       string
		pcmBytes   int
		sampleRate int
		want       int
	}{
		{"one second at 16kHz", 32000, 16000, 1000},
		{"half second at 16kHz", 16000, 16000, 500},
		{"empty", 0, 16000, 0},
		{"zero sample rate guards against divide by zero", 32000, 0, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pcm := make([]byte, tt.pcmBytes)
			if got := durationMS(pcm, tt.sampleRate); got != tt.want {
				t.Errorf("durationMS() = %d, want %d", got, tt.want)
			}
		})
	}
}
