package stt

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// Whisper shells out to whisper.cpp's `whisper-cli`. No CGo, no Python.
//
// Setup:
//
//	git clone https://github.com/ggml-org/whisper.cpp && cd whisper.cpp && make
//	# put whisper-cli on PATH (or set BUDDY_WHISPER_BIN to its full path)
//	./models/download-ggml-model.sh tiny.en   # fast track
//	./models/download-ggml-model.sh large-v3  # slow/quality track
//
// Swap this for the CGo bindings later if you want in-process inference; the
// Recognizer interface stays the same.
type Whisper struct {
	Bin   string // "whisper-cli"
	Model string // path to ggml-*.bin
	label string
}

func NewWhisper(label, bin, model string) *Whisper {
	return &Whisper{Bin: bin, Model: model, label: label}
}

func (w *Whisper) Name() string { return "whisper(" + w.label + ")" }

func (w *Whisper) Transcribe(ctx context.Context, pcm []byte) (Result, error) {
	// whisper-cli reads a WAV file; wrap the raw PCM in a WAV header.
	tmp, err := os.CreateTemp("", "buddy-*.wav")
	if err != nil {
		return Result{}, err
	}
	defer os.Remove(tmp.Name())
	if err := writeWAV(tmp, pcm, 16000, 1); err != nil {
		tmp.Close()
		return Result{}, err
	}
	tmp.Close()

	// -nt: no timestamps, -otxt off, print to stdout.
	cmd := exec.CommandContext(ctx, w.Bin,
		"-m", w.Model,
		"-f", tmp.Name(),
		"-nt",
		"-l", "en",
		"--no-prints",
	)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return Result{}, fmt.Errorf("whisper-cli: %w: %s", err, errb.String())
	}
	return Result{Text: strings.TrimSpace(out.String()), Confidence: 0.9}, nil
}

// writeWAV emits a minimal 16-bit PCM WAV around the raw samples.
func writeWAV(f *os.File, pcm []byte, sampleRate, channels int) error {
	const bitsPerSample = 16
	byteRate := sampleRate * channels * bitsPerSample / 8
	blockAlign := channels * bitsPerSample / 8
	dataLen := len(pcm)

	var h bytes.Buffer
	h.WriteString("RIFF")
	binary.Write(&h, binary.LittleEndian, uint32(36+dataLen))
	h.WriteString("WAVE")
	h.WriteString("fmt ")
	binary.Write(&h, binary.LittleEndian, uint32(16))            // subchunk size
	binary.Write(&h, binary.LittleEndian, uint16(1))             // PCM
	binary.Write(&h, binary.LittleEndian, uint16(channels))      //
	binary.Write(&h, binary.LittleEndian, uint32(sampleRate))    //
	binary.Write(&h, binary.LittleEndian, uint32(byteRate))      //
	binary.Write(&h, binary.LittleEndian, uint16(blockAlign))    //
	binary.Write(&h, binary.LittleEndian, uint16(bitsPerSample)) //
	h.WriteString("data")
	binary.Write(&h, binary.LittleEndian, uint32(dataLen))

	if _, err := f.Write(h.Bytes()); err != nil {
		return err
	}
	_, err := f.Write(pcm)
	return err
}
