// Package config loads runtime configuration from environment variables.
// Every value has a sane default so the server boots with zero setup.
package config

import (
	"os"
	"strings"
)

type Config struct {
	Env  string // "dev" | "prod"
	Addr string // listen address, e.g. ":8080"

	// static / proxy
	WebDist string // dir served in prod
	ViteURL string // reverse-proxy target in dev

	// STT
	FastSTT          string // "mock" | "whisper"
	SlowSTT          string // "mock" | "whisper"
	WhisperBin       string
	WhisperFastModel string
	WhisperSlowModel string

	// LLM (Ollama)
	OllamaURL          string
	OllamaChatModel    string
	OllamaCorrectModel string
}

func Load() Config {
	return Config{
		Env:  env("BUDDY_ENV", "dev"),
		Addr: env("BUDDY_ADDR", ":8080"),

		WebDist: env("BUDDY_WEB_DIST", "apps/web/dist"),
		ViteURL: env("BUDDY_VITE_URL", "http://localhost:5173"),

		FastSTT:          env("BUDDY_FAST_STT", "mock"),
		SlowSTT:          env("BUDDY_SLOW_STT", "mock"),
		WhisperBin:       env("BUDDY_WHISPER_BIN", "whisper-cli"),
		WhisperFastModel: env("BUDDY_WHISPER_FAST_MODEL", "models/ggml-tiny.en.bin"),
		WhisperSlowModel: env("BUDDY_WHISPER_SLOW_MODEL", "models/ggml-large-v3.bin"),

		OllamaURL:          env("BUDDY_OLLAMA_URL", "http://localhost:11434"),
		OllamaChatModel:    env("BUDDY_OLLAMA_CHAT_MODEL", "llama3.2:3b"),
		OllamaCorrectModel: env("BUDDY_OLLAMA_CORRECT_MODEL", "llama3.2:3b"),
	}
}

func (c Config) IsDev() bool { return strings.ToLower(c.Env) != "prod" }

func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}
