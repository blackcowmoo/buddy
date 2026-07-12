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

	pipe := &pipeline.Pipeline{
		FastSTT:            buildSTT(cfg.FastSTT, "fast", cfg),
		SlowSTT:            buildSTT(cfg.SlowSTT, "slow", cfg),
		LLM:                llm.NewOpenAI(cfg.LLMBaseURL, cfg.LLMAPIKey),
		ChatModel:          cfg.LLMChatModel,
		CorrectModel:       cfg.LLMCorrectModel,
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
	// This is a separate, unrelated feature from the recording archive below
	// — see internal/recording's package doc for why the two coexist.
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

	// Voice recording archive: optional, disabled unless BUDDY_S3_BUCKET is
	// set (see config.RecordingS3Bucket and internal/recording). Shares st's
	// MySQL pools rather than opening a second connection to the same
	// instance.
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
// skips saving) when BUDDY_S3_BUCKET is unset — the same optional-feature
// convention as buildIdentity's Redis cache.
func buildRecordingStore(ctx context.Context, cfg config.Config, st *store.MySQLStore) recording.Store {
	if cfg.RecordingS3Bucket == "" {
		return nil
	}
	rw, ro := st.DB()
	rec, err := recording.NewS3(ctx, cfg.RecordingS3Bucket, cfg.RecordingS3Region, cfg.RecordingS3Endpoint, rw, ro)
	if err != nil {
		log.Fatalf("recording store: %v", err)
	}
	return rec
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
