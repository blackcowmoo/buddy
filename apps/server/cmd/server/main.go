// Command server is the composition root: it reads config, builds the engines,
// and starts the single HTTP entry point.
package main

import (
	"context"
	"io"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"buddy/server/internal/audiostore"
	"buddy/server/internal/config"
	"buddy/server/internal/httpserver"
	"buddy/server/internal/identity"
	"buddy/server/internal/llm"
	"buddy/server/internal/pipeline"
	"buddy/server/internal/recording"
	"buddy/server/internal/store"
	"buddy/server/internal/stt"
	"buddy/server/internal/transport"
	"buddy/server/internal/webassets"

	"github.com/redis/go-redis/v9"
)

func main() {
	log.SetFlags(log.Ltime)
	cfg := config.Load()

	fastSTT, slowSTT := buildSTT(cfg)
	pipe := &pipeline.Pipeline{
		FastSTT:            fastSTT,
		SlowSTT:            slowSTT,
		LLM:                llm.NewOpenAI(cfg.LLMChatURL, cfg.LLMAPIKey),
		ChatModel:          cfg.LLMChatModel,
		Analysis:           buildAnalysisCandidates(cfg),
		Judge:              llm.NewOpenAI(cfg.LLMJudgeURL, cfg.LLMAPIKey),
		JudgeModel:         cfg.LLMJudgeModel,
		FeedbackLang:       cfg.FeedbackLang,
		MaxHistoryMessages: cfg.MaxHistoryMessages,
	}

	// Persistent per-user memory.
	ident, identCloser := buildIdentity(context.Background(), cfg)
	defer identCloser.Close()
	st, err := store.NewMySQL(store.MySQLConfig{
		RWHost:   cfg.MySQLRWHost,
		ROHost:   cfg.MySQLROHost,
		Port:     cfg.MySQLPort,
		User:     cfg.MySQLUser,
		Password: cfg.MySQLPassword,
		Database: cfg.MySQLDatabase,
	})
	if err != nil {
		log.Fatalf("store: %v", err)
	}
	defer st.Close()

	// Temporary audio backup: only enabled once an endpoint is configured, so
	// the server still boots with zero setup by default (see internal/audiostore).
	// A second, independent feature (the recording archive below) archives
	// the same utterance audio to the same S3_* endpoint/credentials for a
	// different purpose — see config.Config's doc comment for why both share
	// one config block.
	var audio transport.AudioSaver
	if cfg.S3Endpoint != "" {
		as, err := audiostore.New(audiostore.Config{
			Endpoint:     cfg.S3Endpoint,
			PathStyle:    cfg.S3PathStyle,
			AccessKey:    cfg.S3AccessKey,
			SecretKey:    cfg.S3SecretKey,
			Bucket:       cfg.S3Bucket,
			StorageClass: cfg.S3StorageClass,
		})
		if err != nil {
			log.Fatalf("audiostore: %v", err)
		}
		audio = as
	}

	// Voice recording archive: optional, disabled unless S3Bucket is set (see
	// internal/recording). Shares st's MySQL pools rather than opening a
	// second connection to the same instance.
	recordings := buildRecordingStore(context.Background(), cfg, st)
	if recordings != nil {
		defer recordings.Close()
	}

	srv := httpserver.New(cfg, pipe, webassets.FS(), ident, st, audio, recordings)

	go func() {
		log.Printf("buddy up on %s  env=%s  fast=%s  slow=%s  feedback=%s",
			cfg.Addr, cfg.Env, pipe.FastSTT.Name(), pipe.SlowSTT.Name(), cfg.FeedbackLang)
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

// buildIdentity selects how learners are identified. "cookie" needs zero
// setup (local dev); "oidc" verifies the Dex-issued JWT in the Authorization
// header directly against Dex — see internal/identity/oidc.go. When
// REDIS_CLUSTER_HOST is set, "oidc" results are additionally cached in a
// Redis Cluster (internal/identity/cached_oidc.go) so the auth path stays
// fast even if Dex is slow or briefly unavailable; leaving it unset keeps
// today's behavior of verifying directly every time. The returned io.Closer
// releases whatever resources were opened (the Redis client, if any) and is
// always safe to defer-close, even when it's a no-op.
func buildIdentity(ctx context.Context, cfg config.Config) (identity.Identifier, io.Closer) {
	if cfg.IdentityMode != "oidc" {
		return identity.NewCookieIdentifier(), io.NopCloser(nil)
	}
	ident, err := identity.NewOIDCIdentifier(ctx, cfg.OIDCIssuerURL, cfg.OIDCClientID)
	if err != nil {
		log.Fatalf("oidc identity: %v", err)
	}
	if cfg.RedisClusterHost == "" {
		return ident, io.NopCloser(nil)
	}
	rdb := redis.NewClusterClient(&redis.ClusterOptions{
		// One seed node is enough: go-redis discovers the rest of the
		// cluster's topology (CLUSTER SHARDS) from it.
		Addrs:    []string{store.HostPort(cfg.RedisClusterHost, cfg.RedisPort)},
		Password: cfg.RedisPassword,
	})
	return identity.NewCachedOIDCIdentifier(ident, rdb), rdb
}

// buildRecordingStore builds the voice-recording archive (internal/recording)
// when configured, or returns nil (archival disabled, the WS handler simply
// skips saving) when S3Bucket is unset — the same optional-feature
// convention as buildIdentity's Redis cache. Shares cfg's S3_* settings with
// internal/audiostore (see config.Config's doc comment for why the two
// features use the same endpoint/credentials).
func buildRecordingStore(ctx context.Context, cfg config.Config, st *store.MySQLStore) recording.Store {
	if cfg.S3Bucket == "" {
		return nil
	}
	rw, ro := st.DB()
	rec, err := recording.NewS3(ctx, recording.S3Config{
		Endpoint:     cfg.S3Endpoint,
		PathStyle:    cfg.S3PathStyle,
		AccessKey:    cfg.S3AccessKey,
		SecretKey:    cfg.S3SecretKey,
		Bucket:       cfg.S3Bucket,
		StorageClass: cfg.S3StorageClass,
	}, rw, ro)
	if err != nil {
		log.Fatalf("recording store: %v", err)
	}
	return rec
}

// buildSTT selects the STT engine(s) for the fast and refine tracks. A
// server engine (cfg.STTEngine — WHISPER_SERVER_URLS, PARAKEET_SERVER_URLS,
// ...; see config.sttEngines) takes priority when configured: it's one
// model/deployment, so the same recognizer instance serves both tracks
// (its URLs already round-robin across replicas — see stt.HTTPTranscriber).
// Otherwise falls back to the legacy per-track switch: "mock" (zero setup)
// or "whisper" (whisper.cpp subprocess, internal/stt/whisper.go).
func buildSTT(cfg config.Config) (fast, slow stt.Recognizer) {
	if cfg.STTEngine != "" {
		r := stt.NewHTTPTranscriber(cfg.STTEngine, cfg.STTURLs, cfg.STTModel)
		return r, r
	}
	return buildLegacySTT(cfg.FastSTT, "fast", cfg), buildLegacySTT(cfg.SlowSTT, "slow", cfg)
}

func buildLegacySTT(kind, label string, cfg config.Config) stt.Recognizer {
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

// buildAnalysisCandidates builds one ensemble candidate per
// BUDDY_LLM_ANALYSIS_URLS entry, paired by index with
// BUDDY_LLM_ANALYSIS_MODELS — a shorter model list repeats its last entry
// for the remaining URLs (the common case: one model replicated across
// several server instances). See pipeline.Pipeline.Analysis/analyze().
func buildAnalysisCandidates(cfg config.Config) []pipeline.Candidate {
	cands := make([]pipeline.Candidate, len(cfg.LLMAnalysisURLs))
	for i, u := range cfg.LLMAnalysisURLs {
		model := cfg.LLMAnalysisModels[len(cfg.LLMAnalysisModels)-1]
		if i < len(cfg.LLMAnalysisModels) {
			model = cfg.LLMAnalysisModels[i]
		}
		cands[i] = pipeline.Candidate{LLM: llm.NewOpenAI(u, cfg.LLMAPIKey), Model: model}
	}
	return cands
}
