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
	"buddy/server/internal/newsarticle"
	"buddy/server/internal/pipeline"
	"buddy/server/internal/recording"
	"buddy/server/internal/store"
	"buddy/server/internal/stt"
	"buddy/server/internal/transport"
	"buddy/server/internal/tts"
	"buddy/server/internal/ttsstore"
	"buddy/server/internal/webassets"
	"buddy/server/internal/wordreview"
	"buddy/server/internal/writing"

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

	// Shared Redis Cluster client, backing several independent optional
	// features below (OIDC verification cache, translation backfill
	// queue/lock, the model-call cache just below) — all come up, or all
	// stay disabled, together based on REDIS_CLUSTER_HOST, rather than each
	// opening its own connection.
	rdb, redisCloser := buildRedis(cfg)
	defer redisCloser.Close()

	// Cache Analysis/Judge/STT candidates' external calls in Redis, keyed by
	// model+input (see llm.NewCached/stt.NewCached). This is what makes an
	// asyncjob reap-retry (worker crash/OOM/redeploy — jobs always rerun
	// "from scratch", see that package's doc comment) cheap: a candidate
	// that already succeeded on the failed attempt is served from cache
	// instead of being called again, so only the candidate(s) that actually
	// failed do real work the second time. pipe.LLM (the streamed chat
	// reply) is deliberately left unwrapped — see llm.CachedClient's doc
	// comment.
	if rdb != nil {
		pipe.Judge = llm.NewCached(pipe.Judge, rdb, llm.DefaultCacheTTL)
		for i := range pipe.Analysis {
			pipe.Analysis[i].LLM = llm.NewCached(pipe.Analysis[i].LLM, rdb, llm.DefaultCacheTTL)
		}
		for i := range pipe.STT {
			pipe.STT[i] = stt.NewCached(pipe.STT[i], rdb, stt.DefaultCacheTTL)
		}
	}

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

	// Word-review (spaced-repetition study list): unlike recordings below,
	// this has no external dependency beyond MySQL, so it's always on —
	// shares st's pools the same way. Built here, ahead of the correction
	// hook below, because captureCorrectionWords (see
	// transport.CorrectionJobHandler) needs it to auto-capture
	// vocabulary/phrasing corrections into the same study list.
	wordReviews := buildWordReviewStore(context.Background(), st)
	defer wordReviews.Close()

	// "오늘의 아티클" (news article reading + quiz): same "no external
	// dependency beyond MySQL, always on" convention as wordReviews above.
	articles := buildNewsArticleStore(context.Background(), st)
	defer articles.Close()
	writingStore := buildWritingStore(context.Background(), st)
	defer writingStore.Close()

	writingQueue := startWorker(rdb, jobsCtx, asyncjob.KindWritingPrompt, transport.WritingWorkerConcurrency, transport.WritingClaimTTL,
		transport.WritingJobHandler(pipe, writingStore, st.GetLearnerProfile))

	// "오늘의 아티클" read-aloud + (on demand) chat message read-aloud TTS:
	// optional, disabled unless both BUDDY_TTS_URL and S3Bucket are set (see
	// buildArticleAudio) — same "zero setup by default" convention as
	// recordings below.
	articleAudio := buildArticleAudio(context.Background(), cfg, st)

	// Word verification: fact-checks a word/phrase right after "학습하기"
	// saves it Pending (see httpserver.wordSaveHandler) or after a
	// vocabulary/phrasing correction auto-captures it (see
	// transport.captureCorrectionWords), same "optional durable queue,
	// inline-goroutine fallback when Redis isn't configured" convention as
	// studySummaryQueue/studyQuizQueue below.
	wordVerifyQueue := startWorker(rdb, jobsCtx, asyncjob.KindWordVerify, transport.WordVerifyWorkerConcurrency, transport.WordVerifyClaimTTL,
		transport.WordVerifyJobHandler(pipe, wordReviews))

	// Article-study generation: summarizes + quizzes a freshly drawn "오늘의
	// 아티클" story in the background (see httpserver.articleDrawHandler),
	// same "optional durable queue, inline-goroutine fallback when Redis
	// isn't configured" convention as wordVerifyQueue above — this is what
	// lets the draw survive the learner navigating away before generation
	// finishes.
	articleStudyQueue := startWorker(rdb, jobsCtx, asyncjob.KindArticleStudy, transport.ArticleStudyWorkerConcurrency, transport.ArticleStudyClaimTTL,
		transport.ArticleStudyJobHandler(pipe, articles, articleAudio))
	// DB-only orphan sweep for article-study generation: runs regardless of
	// whether Redis/articleStudyQueue is configured, since it's the only
	// thing that resumes a draw abandoned mid-generation (crash, OOM, or a
	// redeploy killing the in-process fallback goroutine EnqueueOrRunInline
	// uses when articleStudyQueue is nil) when there's no durable Redis claim
	// to reap in the first place — see transport.RunArticleStudySweepLoop.
	go transport.RunArticleStudySweepLoop(jobsCtx, articleStudyQueue, pipe, articles, articleAudio)

	// Word auto-add generation: generates a batch of new words fit to the
	// learner's profile in the background (see httpserver.wordAutoAddHandler),
	// same "optional durable queue, inline-goroutine fallback when Redis
	// isn't configured" convention as wordVerifyQueue above — this is what
	// lets "새 단어 추가로 학습하기" survive the learner navigating away before
	// generation finishes.
	wordAutoAddQueue := startWorker(rdb, jobsCtx, asyncjob.KindWordAutoAdd, transport.WordAutoAddWorkerConcurrency, transport.WordAutoAddClaimTTL,
		transport.WordAutoAddJobHandler(pipe, wordReviews, st, wordVerifyQueue))

	wordDefineQueue := startWorker(rdb, jobsCtx, asyncjob.KindWordDefine, transport.WordDefineWorkerConcurrency, transport.WordDefineClaimTTL,
		transport.WordDefineJobHandler(pipe, rdb))
	wordResearchQueue := startWorker(rdb, jobsCtx, asyncjob.KindWordResearch, transport.WordResearchWorkerConcurrency, transport.WordResearchClaimTTL,
		transport.WordResearchJobHandler(pipe, wordReviews))

	// End-of-conversation wrap-up: unlike the reply/correction/translation/
	// title hooks below, this isn't wired onto pipe (nothing mid-conversation
	// triggers it directly) — httpserver's sessionEndHandler and
	// transport.CorrectionJobHandler's own instant-session auto-finalize
	// (see transport.FinalizeSession) both call
	// transport.EnqueueStudySummaryJob once a room is frozen, which is why
	// this has to be built before the correction queue below. studySummaryQueue
	// stays nil, same "optional feature, zero setup by default" convention,
	// when Redis isn't configured; both callers fall back to running it
	// inline on their own detached goroutine in that case.
	studySummaryQueue := startWorker(rdb, jobsCtx, asyncjob.KindStudySummary, transport.StudySummaryWorkerConcurrency, transport.StudySummaryClaimTTL,
		transport.StudySummaryJobHandler(pipe, st))

	// Practice-quiz pre-generation: runs independently of, but is enqueued
	// alongside, the wrap-up above (see transport.FinalizeSession) — same
	// "optional feature, zero setup by default" convention.
	studyQuizQueue := startWorker(rdb, jobsCtx, asyncjob.KindStudyQuiz, transport.StudyQuizWorkerConcurrency, transport.StudyQuizClaimTTL,
		transport.StudyQuizJobHandler(pipe, st))

	if rdb != nil {
		replyQueue := startWorker(rdb, jobsCtx, asyncjob.KindReply, transport.ReplyWorkerConcurrency, transport.ReplyClaimTTL,
			transport.ReplyJobHandler(pipe, st, nil, nil))
		pipe.ReplyHook = transport.NewReplyHook(pipe, st, replyQueue)

		correctionQueue := startWorker(rdb, jobsCtx, asyncjob.KindCorrection, transport.CorrectionWorkerConcurrency, transport.CorrectionClaimTTL,
			transport.CorrectionJobHandler(pipe, st, wordReviews, wordVerifyQueue, studySummaryQueue, studyQuizQueue, nil))
		pipe.CorrectHook = transport.NewCorrectHook(pipe, st, wordReviews, wordVerifyQueue, studySummaryQueue, studyQuizQueue, correctionQueue)

		liveTranslationQueue := startWorker(rdb, jobsCtx, asyncjob.KindLiveTranslation, transport.LiveTranslationWorkerConcurrency, transport.LiveTranslationClaimTTL,
			transport.TranslationJobHandler(pipe, st, nil))
		pipe.TranslateHook = transport.NewTranslateHook(pipe, st, liveTranslationQueue)

		titleQueue := startWorker(rdb, jobsCtx, asyncjob.KindTitle, transport.TitleWorkerConcurrency, transport.TitleClaimTTL,
			transport.TitleJobHandler(pipe, st))
		pipe.TitleHook = transport.NewTitleHook(pipe, st, titleQueue)
	}

	// Learner-profile rebuild: enqueued by httpserver.sessionDeleteHandler
	// when a deleted session had actually folded a study summary into the
	// profile — same "optional feature, zero setup by default" convention.
	profileRegenerateQueue := startWorker(rdb, jobsCtx, asyncjob.KindProfileRegenerate, transport.ProfileRegenerateWorkerConcurrency, transport.ProfileRegenerateClaimTTL,
		transport.ProfileRegenerateJobHandler(pipe, st))

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

	srv := httpserver.New(cfg, httpserver.Dependencies{
		Pipeline:     pipe,
		Assets:       webassets.FS(),
		Identity:     ident,
		Store:        st,
		AudioBackup:  audio,
		Recordings:   recordings,
		Words:        wordReviews,
		Articles:     articles,
		Writing:      writingStore,
		ArticleAudio: articleAudio,
		Redis:        rdb,
		Queues: httpserver.JobQueues{
			WordVerify:        wordVerifyQueue,
			Translate:         translateQueue,
			Correction:        correctionBackfillQueue,
			StudySummary:      studySummaryQueue,
			StudyQuiz:         studyQuizQueue,
			ProfileRegenerate: profileRegenerateQueue,
			ArticleStudy:      articleStudyQueue,
			Writing:           writingQueue,
			WordAutoAdd:       wordAutoAddQueue,
			WordDefine:        wordDefineQueue,
			WordResearch:      wordResearchQueue,
		},
	})

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

// startWorker builds and starts an optional Redis-backed worker. Keeping the
// nil check here lets the composition root declare every queue uniformly.
func startWorker(rdb redis.UniversalClient, jobsCtx context.Context, kind asyncjob.Kind, concurrency int, claimTTL time.Duration, handler asyncjob.Handler) *asyncjob.Queue {
	if rdb == nil {
		return nil
	}
	queue := asyncjob.NewQueue(rdb)
	go asyncjob.NewWorker(rdb, kind, concurrency, claimTTL, handler).Run(jobsCtx)
	return queue
}

// buildRedis constructs the shared Redis Cluster client used by two
// independent optional features — buildIdentity's OIDC verification cache
// and the translation backfill queue/lock (internal/backfill) — so both come
// up, or both stay disabled, together based on the same REDIS_CLUSTER_HOST
// config, instead of each opening its own connection. Returns a nil client
// (optional features and startWorker handle that) and a no-op closer when
// RedisClusterHost is unset; the returned io.Closer is always safe to
// defer-close either way.
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

// buildArticleAudio builds the "오늘의 아티클"/chat message read-aloud TTS
// pipeline (internal/tts's Kokoro-FastAPI client + internal/ttsstore's S3
// cache) when both BUDDY_TTS_URL and S3Bucket are set, or returns nil
// (read-aloud generation disabled — articleDrawHandler simply skips
// pre-generation and articleAudioHandler answers 503) otherwise, the same
// optional-feature convention as buildRecordingStore. Shares cfg's S3_*
// settings and st's MySQL pools with internal/recording — see
// config.Config's TTSURL doc comment for why generated audio piggybacks on
// the same bucket rather than needing its own.
func buildArticleAudio(ctx context.Context, cfg config.Config, st *store.MySQLStore) *transport.ArticleAudio {
	if cfg.TTSURL == "" || cfg.S3Bucket == "" {
		return nil
	}
	rw, ro := st.DB()
	cache, err := ttsstore.New(ctx, ttsstore.Config{
		Endpoint:     cfg.S3Endpoint,
		PathStyle:    cfg.S3PathStyle,
		AccessKey:    cfg.S3AccessKey,
		SecretKey:    cfg.S3SecretKey,
		Bucket:       cfg.S3Bucket,
		StorageClass: cfg.S3StorageClass,
	}, rw, ro)
	if err != nil {
		log.Fatalf("tts cache: %v", err)
	}
	return &transport.ArticleAudio{
		Client: tts.NewKokoro(cfg.TTSURL, cfg.TTSVoice, cfg.TTSVolumeMultiplier, ""),
		Cache:  cache,
	}
}

// buildWordReviewStore builds the spaced-repetition study-list store
// (internal/wordreview). Unlike buildRecordingStore, this has no optional
// external dependency (no S3, just MySQL) so it's constructed unconditionally
// — shares st's pools the same way.
func buildWordReviewStore(ctx context.Context, st *store.MySQLStore) *wordreview.MySQLStore {
	rw, ro := st.DB()
	words, err := wordreview.NewMySQL(ctx, rw, ro)
	if err != nil {
		log.Fatalf("word review store: %v", err)
	}
	return words
}

// buildNewsArticleStore builds the "오늘의 아티클" store (internal/newsarticle).
// Unlike buildRecordingStore, this has no optional external dependency (no
// S3, just MySQL) so it's constructed unconditionally — shares st's pools
// the same way as buildWordReviewStore.
func buildNewsArticleStore(ctx context.Context, st *store.MySQLStore) *newsarticle.MySQLStore {
	rw, ro := st.DB()
	articles, err := newsarticle.NewMySQL(ctx, rw, ro)
	if err != nil {
		log.Fatalf("news article store: %v", err)
	}
	return articles
}

func buildWritingStore(ctx context.Context, st *store.MySQLStore) *writing.MySQLStore {
	rw, ro := st.DB()
	result, err := writing.NewMySQL(ctx, rw, ro)
	if err != nil {
		log.Fatalf("writing store: %v", err)
	}
	return result
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
