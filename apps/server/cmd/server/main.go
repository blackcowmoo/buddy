// Command server is the composition root: it reads config, builds the engines,
// and starts the single HTTP entry point.
package main

import (
	"context"
	"io"
	"log"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"buddy/server/internal/asyncjob"
	"buddy/server/internal/audiostore"
	"buddy/server/internal/backfill"
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
		STT:                buildSTT(cfg),
		LLM:                llm.NewOpenAI(cfg.LLMChatURL, cfg.LLMAPIKey),
		ChatModel:          cfg.LLMChatModel,
		Analysis:           buildAnalysisCandidates(cfg),
		Judge:              llm.NewOpenAI(cfg.LLMJudgeURL, cfg.LLMAPIKey),
		JudgeModel:         cfg.LLMJudgeModel,
		FeedbackLang:       cfg.FeedbackLang,
		MaxHistoryMessages: cfg.MaxHistoryMessages,
	}

	// Shared Redis Cluster client, backing two independent optional
	// features below (OIDC verification cache, translation backfill
	// queue/lock) — both come up, or both stay disabled, together based on
	// REDIS_CLUSTER_HOST, rather than each opening its own connection.
	rdb, redisCloser := buildRedis(cfg)
	defer redisCloser.Close()

	// Persistent per-user memory. Identity verification and the MySQL
	// connection/schema bootstrap don't depend on each other, so they run
	// concurrently rather than paying two sequential network round trips at
	// startup.
	var ident identity.Identifier
	var st *store.MySQLStore
	var storeErr error
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		ident = buildIdentity(context.Background(), cfg, rdb)
	}()
	go func() {
		defer wg.Done()
		st, storeErr = store.NewMySQL(store.MySQLConfig{
			RWHost:   cfg.MySQLRWHost,
			ROHost:   cfg.MySQLROHost,
			Port:     cfg.MySQLPort,
			User:     cfg.MySQLUser,
			Password: cfg.MySQLPassword,
			Database: cfg.MySQLDatabase,
		})
	}()
	wg.Wait()
	if storeErr != nil {
		log.Fatalf("store: %v", storeErr)
	}
	defer st.Close()

	// Translation/correction backfill: fills in the native-language
	// translation, or grammar-correction result, for turns that never got one
	// (see internal/backfill's doc comment). Disabled (translateQueue/
	// correctionQueue stay nil, Enqueue becomes a no-op) unless Redis is
	// configured — same "zero setup by default" convention as
	// audio/recordings below.
	backfillCtx, backfillCancel := context.WithCancel(context.Background())
	defer backfillCancel()
	var translateQueue *backfill.Queue
	var correctionBackfillQueue *backfill.CorrectionQueue
	if rdb != nil {
		translateQueue = backfill.NewQueue(rdb)
		go backfill.NewWorker(rdb, st, pipe).Run(backfillCtx)

		correctionBackfillQueue = backfill.NewCorrectionQueue(rdb)
		go backfill.NewCorrectionWorker(rdb, st, pipe).Run(backfillCtx)
	}

	// Durable background work: makes chat replies, grammar correction, live
	// translation, and title generation all survive both the learner's
	// connection disconnecting and this replica dying mid-task (see
	// internal/asyncjob's package doc and transport.New*Hook). Disabled
	// (every *Hook stays nil, so the pipeline calls its LLM directly
	// in-process exactly as before this existed) unless Redis is
	// configured — same convention as the translation backfill queue
	// above. Compaction is deliberately NOT included: it only mutates
	// connection-local in-memory session state that doesn't survive a
	// crash anyway (see transport.NewReplyHook's sibling doc comments and
	// the plan this was built from), so a crash just re-triggers it on the
	// next turn once history grows past the window again.
	jobsCtx, jobsCancel := context.WithCancel(context.Background())
	defer jobsCancel()
	var titleQueue *asyncjob.Queue
	if rdb != nil {
		replyQueue := asyncjob.NewQueue(rdb)
		pipe.ReplyHook = transport.NewReplyHook(pipe, st, replyQueue)
		go asyncjob.NewWorker(rdb, asyncjob.KindReply, transport.ReplyWorkerConcurrency, transport.ReplyClaimTTL,
			transport.ReplyJobHandler(pipe, st, nil, nil)).Run(jobsCtx)

		correctionQueue := asyncjob.NewQueue(rdb)
		pipe.CorrectHook = transport.NewCorrectHook(pipe, st, correctionQueue)
		go asyncjob.NewWorker(rdb, asyncjob.KindCorrection, transport.CorrectionWorkerConcurrency, transport.CorrectionClaimTTL,
			transport.CorrectionJobHandler(pipe, st, nil)).Run(jobsCtx)

		liveTranslationQueue := asyncjob.NewQueue(rdb)
		pipe.TranslateHook = transport.NewTranslateHook(pipe, st, liveTranslationQueue)
		go asyncjob.NewWorker(rdb, asyncjob.KindLiveTranslation, transport.LiveTranslationWorkerConcurrency, transport.LiveTranslationClaimTTL,
			transport.TranslationJobHandler(pipe, st, nil)).Run(jobsCtx)

		titleQueue = asyncjob.NewQueue(rdb)
		go asyncjob.NewWorker(rdb, asyncjob.KindTitle, transport.TitleWorkerConcurrency, transport.TitleClaimTTL,
			transport.TitleJobHandler(pipe, st)).Run(jobsCtx)
	}

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

	srv := httpserver.New(cfg, pipe, webassets.FS(), ident, st, audio, recordings, translateQueue, correctionBackfillQueue, titleQueue)

	go func() {
		log.Printf("buddy up on %s  env=%s  stt=%v  feedback=%s",
			cfg.Addr, cfg.Env, pipe.STTNames(), cfg.FeedbackLang)
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

// buildRedis constructs the shared Redis Cluster client used by two
// independent optional features — buildIdentity's OIDC verification cache
// and the translation backfill queue/lock (internal/backfill) — so both come
// up, or both stay disabled, together based on the same REDIS_CLUSTER_HOST
// config, instead of each opening its own connection. Returns a nil client
// (every caller's own "optional feature, do nothing" branch handles that)
// and a no-op closer when RedisClusterHost is unset; the returned io.Closer
// is always safe to defer-close either way.
func buildRedis(cfg config.Config) (redis.UniversalClient, io.Closer) {
	if cfg.RedisClusterHost == "" {
		return nil, io.NopCloser(nil)
	}
	rdb := redis.NewClusterClient(&redis.ClusterOptions{
		// One seed node is enough: go-redis discovers the rest of the
		// cluster's topology (CLUSTER SHARDS) from it.
		Addrs:    []string{store.HostPort(cfg.RedisClusterHost, cfg.RedisPort)},
		Password: cfg.RedisPassword,
	})
	return rdb, rdb
}

// buildIdentity selects how learners are identified. "cookie" needs zero
// setup (local dev); "oidc" verifies the Dex-issued JWT in the Authorization
// header directly against Dex — see internal/identity/oidc.go. When rdb is
// non-nil (REDIS_CLUSTER_HOST set — see buildRedis), "oidc" results are
// additionally cached in Redis (internal/identity/cached_oidc.go) so the
// auth path stays fast even if Dex is slow or briefly unavailable; a nil rdb
// keeps today's behavior of verifying directly every time.
func buildIdentity(ctx context.Context, cfg config.Config, rdb redis.UniversalClient) identity.Identifier {
	if cfg.IdentityMode != "oidc" {
		return identity.NewCookieIdentifier()
	}
	ident, err := identity.NewOIDCIdentifier(ctx, cfg.OIDCIssuerURL, cfg.OIDCClientID)
	if err != nil {
		log.Fatalf("oidc identity: %v", err)
	}
	if rdb == nil {
		return ident
	}
	return identity.NewCachedOIDCIdentifier(ident, rdb)
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

// buildSTT builds the STT ensemble for pipeline.Pipeline.STT: one Recognizer
// per configured server engine (cfg.STTEngines — WHISPER_SERVER_URLS,
// PARAKEET_SERVER_URLS, ...; see config.sttEngines), all called concurrently
// per utterance (see pipeline.Pipeline.transcribe) — each engine's own
// entries still round-robin internally across that engine's replicas (see
// stt.HTTPTranscriber). Falls back to the legacy fast+slow pair (mock/
// subprocess whisper, internal/stt/whisper.go) as a two-member ensemble when
// no server engine is configured at all.
func buildSTT(cfg config.Config) []stt.Recognizer {
	if len(cfg.STTEngines) > 0 {
		recs := make([]stt.Recognizer, len(cfg.STTEngines))
		for i, e := range cfg.STTEngines {
			recs[i] = stt.NewHTTPTranscriber(e.Name, e.URLs, e.Models)
		}
		return recs
	}
	return []stt.Recognizer{
		buildLegacySTT(cfg.FastSTT, "fast", cfg.WhisperFastModel, 150*time.Millisecond, cfg),
		buildLegacySTT(cfg.SlowSTT, "slow", cfg.WhisperSlowModel, 600*time.Millisecond, cfg), // pretend the quality model is slower
	}
}

func buildLegacySTT(kind, label, whisperModel string, mockDelay time.Duration, cfg config.Config) stt.Recognizer {
	switch kind {
	case "whisper":
		return stt.NewWhisper(label, cfg.WhisperBin, whisperModel)
	default:
		return stt.NewMock(label, mockDelay)
	}
}

// buildAnalysisCandidates builds one ensemble candidate per
// BUDDY_LLM_ANALYSIS_URLS entry ("model@url" pairs — see
// config.parseModelURLPairs), so LLMAnalysisURLs/LLMAnalysisModels are
// always the same length, one candidate per pair. See
// pipeline.Pipeline.Analysis/analyze().
func buildAnalysisCandidates(cfg config.Config) []pipeline.Candidate {
	cands := make([]pipeline.Candidate, len(cfg.LLMAnalysisURLs))
	for i, u := range cfg.LLMAnalysisURLs {
		cands[i] = pipeline.Candidate{LLM: llm.NewOpenAI(u, cfg.LLMAPIKey), Model: cfg.LLMAnalysisModels[i]}
	}
	return cands
}
