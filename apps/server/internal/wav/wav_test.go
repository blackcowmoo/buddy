package wav

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func TestEncode(t *testing.T) {
	pcm := make([]byte, 32000) // 1 second of mono 16-bit @ 16kHz
	var out bytes.Buffer
	if err := Encode(&out, pcm, 16000, 1); err != nil {
		t.Fatalf("Encode() error = %v", err)
	}
	got := out.Bytes()

	if len(got) != 44+len(pcm) {
		t.Fatalf("len(got) = %d, want %d", len(got), 44+len(pcm))
	}
	if string(got[0:4]) != "RIFF" || string(got[8:12]) != "WAVE" {
		t.Fatalf("missing RIFF/WAVE markers: %q", got[0:12])
	}
	if string(got[12:16]) != "fmt " || string(got[36:40]) != "data" {
		t.Fatalf("missing fmt /data chunk markers")
	}
	if v := binary.LittleEndian.Uint16(got[20:22]); v != 1 {
		t.Errorf("audio format = %d, want 1 (PCM)", v)
	}
	if v := binary.LittleEndian.Uint16(got[22:24]); v != 1 {
		t.Errorf("channels = %d, want 1", v)
	}
	if v := binary.LittleEndian.Uint32(got[24:28]); v != 16000 {
		t.Errorf("sample rate = %d, want 16000", v)
	}
	if v := binary.LittleEndian.Uint16(got[34:36]); v != 16 {
		t.Errorf("bits per sample = %d, want 16", v)
	}
	if v := binary.LittleEndian.Uint32(got[40:44]); v != uint32(len(pcm)) {
		t.Errorf("data chunk size = %d, want %d", v, len(pcm))
	}
	if !bytes.Equal(got[44:], pcm) {
		t.Error("payload after the header does not match the input PCM")
	}
}

func TestEncodeStereo(t *testing.T) {
	pcm := make([]byte, 8)
	var out bytes.Buffer
	if err := Encode(&out, pcm, 8000, 2); err != nil {
		t.Fatalf("Encode() error = %v", err)
	}
	got := out.Bytes()
	if v := binary.LittleEndian.Uint16(got[22:24]); v != 2 {
		t.Errorf("channels = %d, want 2", v)
	}
	if v := binary.LittleEndian.Uint16(got[32:34]); v != 4 {
		t.Errorf("block align = %d, want 4 (2 channels * 2 bytes)", v)
	}
}
