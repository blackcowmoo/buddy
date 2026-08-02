package pipeline

import (
	"context"
	"errors"
	"strings"
	"testing"

	"buddy/server/internal/llm"
	"buddy/server/internal/stt"
)

func TestTranscribeSingleEngineSkipsSynthesis(t *testing.T) {
	chatCalls := 0
	p := &Pipeline{
		STT: []stt.Recognizer{fakeSTT{text: "hello there"}},
		LLM: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
			chatCalls++
			return "should not be called", nil
		}},
	}
	final, candidates, err := p.transcribe(context.Background(), "", nil, []byte("pcm"))
	if err != nil {
		t.Fatalf("transcribe() error = %v", err)
	}
	if final != "hello there" {
		t.Fatalf("final = %q, want %q", final, "hello there")
	}
	if len(candidates) != 1 || candidates[0] != "hello there" {
		t.Fatalf("candidates = %v, want [\"hello there\"]", candidates)
	}
	if chatCalls != 0 {
		t.Fatalf("chat model should not be called for a single STT engine, got %d calls", chatCalls)
	}
}

func TestTranscribeMultipleEnginesSynthesizedByChatModel(t *testing.T) {
	var synthInput string
	p := &Pipeline{
		STT: []stt.Recognizer{
			fakeSTT{text: "i scream"},
			fakeSTT{text: "ice cream"},
		},
		LLM: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
			synthInput = msgs[len(msgs)-1].Content
			return "ice cream", nil
		}},
		ChatModel: "chat-model",
	}
	final, candidates, err := p.transcribe(context.Background(), "", nil, []byte("pcm"))
	if err != nil {
		t.Fatalf("transcribe() error = %v", err)
	}
	if final != "ice cream" {
		t.Fatalf("final = %q, want %q", final, "ice cream")
	}
	if len(candidates) != 2 {
		t.Fatalf("candidates = %v, want both engines' texts", candidates)
	}
	if !strings.Contains(synthInput, "i scream") || !strings.Contains(synthInput, "ice cream") {
		t.Fatalf("synthesis input should include both candidates, got %q", synthInput)
	}
}

func TestTranscribeFallsBackToFirstCandidateOnSynthesisError(t *testing.T) {
	p := &Pipeline{
		STT: []stt.Recognizer{
			fakeSTT{text: "first candidate"},
			fakeSTT{text: "second candidate"},
		},
		LLM: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
			return "", errors.New("chat model down")
		}},
	}
	final, _, err := p.transcribe(context.Background(), "", nil, []byte("pcm"))
	if err != nil {
		t.Fatalf("transcribe() error = %v", err)
	}
	if final != "first candidate" {
		t.Fatalf("final = %q, want the first engine's candidate", final)
	}
}

func TestTranscribeSkipsFailedEngineWithoutSynthesizing(t *testing.T) {
	chatCalls := 0
	p := &Pipeline{
		STT: []stt.Recognizer{
			fakeSTT{err: errors.New("engine down")},
			fakeSTT{text: "only surviving text"},
		},
		LLM: &fakeLLM{complete: func(msgs []llm.Message) (string, error) {
			chatCalls++
			return "unused", nil
		}},
	}
	final, candidates, err := p.transcribe(context.Background(), "", nil, []byte("pcm"))
	if err != nil {
		t.Fatalf("transcribe() error = %v", err)
	}
	if final != "only surviving text" {
		t.Fatalf("final = %q, want %q", final, "only surviving text")
	}
	if len(candidates) != 1 {
		t.Fatalf("candidates = %v, want just the surviving engine's text", candidates)
	}
	if chatCalls != 0 {
		t.Fatalf("chat model should not be called with only one surviving candidate, got %d calls", chatCalls)
	}
}

func TestTranscribeAllEnginesFailReturnsErrorMentioningCause(t *testing.T) {
	p := &Pipeline{
		STT: []stt.Recognizer{
			fakeSTT{err: errors.New("engine A down")},
			fakeSTT{err: errors.New("engine B down")},
		},
	}
	_, _, err := p.transcribe(context.Background(), "", nil, []byte("pcm"))
	if err == nil {
		t.Fatal("expected an error when every STT engine fails")
	}
	if !strings.Contains(err.Error(), "engine A down") || !strings.Contains(err.Error(), "engine B down") {
		t.Fatalf("err = %v, want it to mention both underlying failures", err)
	}
}

func TestTranscribeSilenceAcrossAllEnginesIsNotAnError(t *testing.T) {
	p := &Pipeline{
		STT: []stt.Recognizer{fakeSTT{text: "   "}, fakeSTT{text: ""}},
	}
	final, candidates, err := p.transcribe(context.Background(), "", nil, []byte("pcm"))
	if err != nil {
		t.Fatalf("transcribe() error = %v, want nil (silence is not a failure)", err)
	}
	if final != "" || candidates != nil {
		t.Fatalf("final/candidates = %q/%v, want empty", final, candidates)
	}
}

func TestTranscribeNoEnginesConfiguredReturnsError(t *testing.T) {
	p := &Pipeline{}
	if _, _, err := p.transcribe(context.Background(), "", nil, []byte("pcm")); err == nil {
		t.Fatal("expected an error when no STT engines are configured")
	}
}
