// Command server is the composition root: it reads config, builds the engines,
// and starts the single HTTP entry point.
package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"buddy/server/internal/config"
	"buddy/server/internal/httpserver"
	"buddy/server/internal/llm"
	"buddy/server/internal/pipeline"
	"buddy/server/internal/stt"
	"buddy/server/internal/webassets"
)

func main() {
	log.SetFlags(log.Ltime)
	cfg := config.Load()

	pipe := &pipeline.Pipeline{
		FastSTT:      buildSTT(cfg.FastSTT, "fast", cfg),
		SlowSTT:      buildSTT(cfg.SlowSTT, "slow", cfg),
		LLM:          llm.NewOllama(cfg.OllamaURL),
		ChatModel:    cfg.OllamaChatModel,
		CorrectModel: cfg.OllamaCorrectModel,
	}

	srv := httpserver.New(cfg, pipe, webassets.FS())

	go func() {
		log.Printf("buddy up on %s  env=%s  fast=%s  slow=%s",
			cfg.Addr, cfg.Env, pipe.FastSTT.Name(), pipe.SlowSTT.Name())
		if err := srv.ListenAndServe(); err != nil && err.Error() != "http: Server closed" {
			log.Fatalf("listen: %v", err)
		}
	}()

	// Graceful shutdown.
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	log.Println("shutting down…")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
}

// buildSTT selects an STT engine from config. "mock" needs zero setup;
// "whisper" shells out to whisper.cpp (see internal/stt/whisper.go).
func buildSTT(kind, label string, cfg config.Config) stt.Recognizer {
	switch kind {
	case "whisper":
		model := cfg.WhisperFastModel
		if label == "slow" {
			model = cfg.WhisperSlowModel
		}
		return stt.NewWhisper(label, cfg.WhisperBin, model)
	default:
		delay := 150 * time.Millisecond
		if label == "slow" {
			delay = 600 * time.Millisecond // pretend the quality model is slower
		}
		return stt.NewMock(label, delay)
	}
}
