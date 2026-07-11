// Command server is the composition root: it reads config, builds the engines,
// and starts the single HTTP entry point.
package main

import (
	"context"
	"log"
	"net"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"buddy/server/internal/config"
	"buddy/server/internal/httpserver"
	"buddy/server/internal/identity"
	"buddy/server/internal/llm"
	"buddy/server/internal/pipeline"
	"buddy/server/internal/store"
	"buddy/server/internal/stt"
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
	ident := buildIdentity(context.Background(), cfg)
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

	srv := httpserver.New(cfg, pipe, webassets.FS(), ident, st)

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
// today's behavior of verifying directly every time.
func buildIdentity(ctx context.Context, cfg config.Config) identity.Identifier {
	if cfg.IdentityMode != "oidc" {
		return identity.NewCookieIdentifier()
	}
	ident, err := identity.NewOIDCIdentifier(ctx, cfg.OIDCIssuerURL, cfg.OIDCClientID)
	if err != nil {
		log.Fatalf("oidc identity: %v", err)
	}
	if cfg.RedisClusterHost == "" {
		return ident
	}
	rdb := redis.NewClusterClient(&redis.ClusterOptions{
		// One seed node is enough: go-redis discovers the rest of the
		// cluster's topology (CLUSTER SHARDS) from it.
		Addrs:    []string{hostPort(cfg.RedisClusterHost, cfg.RedisPort)},
		Password: cfg.RedisPassword,
	})
	return identity.NewCachedOIDCIdentifier(ident, rdb)
}

// hostPort composes a host:port for redis.ClusterOptions.Addrs. Guards
// against the same class of incident internal/store.TestHostPort covers for
// MySQL: a secret store might inject the host already as "host:port".
func hostPort(host string, fallbackPort int) string {
	if _, _, err := net.SplitHostPort(host); err == nil {
		return host
	}
	return net.JoinHostPort(host, strconv.Itoa(fallbackPort))
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
