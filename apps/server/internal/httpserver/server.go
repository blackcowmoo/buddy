// Package httpserver wires the single HTTP entry point: WebSocket, JSON API,
// and the frontend (proxied to Vite in dev, served from dist in prod).
package httpserver

import (
	"io/fs"
	"log"
	"net/http"
	"os"
	"time"

	"buddy/server/internal/asyncjob"
	"buddy/server/internal/backfill"
	"buddy/server/internal/config"
	"buddy/server/internal/identity"
	"buddy/server/internal/newsarticle"
	"buddy/server/internal/newsfeed"
	"buddy/server/internal/pipeline"
	"buddy/server/internal/recording"
	"buddy/server/internal/store"
	"buddy/server/internal/transport"
	"buddy/server/internal/wordreview"
	"buddy/server/internal/writing"
	"github.com/redis/go-redis/v9"
)

// JobQueues names each independently configured worker queue. Nil means Redis
// is disabled; handlers use their documented detached inline fallback.
type JobQueues struct {
	WordVerify        *asyncjob.Queue
	Translate         *backfill.Queue
	Correction        *backfill.CorrectionQueue
	StudySummary      *asyncjob.Queue
	StudyQuiz         *asyncjob.Queue
	ProfileRegenerate *asyncjob.Queue
	ArticleStudy      *asyncjob.Queue
	Writing           *asyncjob.Queue
	WordAutoAdd       *asyncjob.Queue
	WordDefine        *asyncjob.Queue
	WordResearch      *asyncjob.Queue
}

// Dependencies is the complete HTTP adapter boundary. Required MySQL-backed
// services are named alongside optional S3/Redis features so composition-root
// wiring cannot silently swap same-typed positional arguments.
type Dependencies struct {
	Pipeline     *pipeline.Pipeline
	Assets       fs.FS
	Identity     identity.Identifier
	Store        store.Store
	AudioBackup  transport.AudioSaver
	Recordings   recording.Store
	Words        wordreview.Store
	Articles     newsarticle.Store
	Writing      writing.Store
	ArticleAudio *transport.ArticleAudio
	Redis        redis.UniversalClient
	Queues       JobQueues
}

// New builds the WebSocket, JSON API, and frontend entry point.
func New(cfg config.Config, deps Dependencies) *http.Server {
	pipe := deps.Pipeline
	assets := deps.Assets
	ident := deps.Identity
	st := deps.Store
	audio := deps.AudioBackup
	recordings := deps.Recordings
	words := deps.Words
	articles := deps.Articles
	writingStore := deps.Writing
	articleAudio := deps.ArticleAudio
	rdb := deps.Redis
	wordVerifyQueue := deps.Queues.WordVerify
	translateQueue := deps.Queues.Translate
	correctionQueue := deps.Queues.Correction
	studySummaryQueue := deps.Queues.StudySummary
	studyQuizQueue := deps.Queues.StudyQuiz
	profileRegenerateQueue := deps.Queues.ProfileRegenerate
	articleStudyQueue := deps.Queues.ArticleStudy
	writingQueue := deps.Queues.Writing
	wordAutoAddQueue := deps.Queues.WordAutoAdd
	wordDefineQueue := deps.Queues.WordDefine
	wordResearchQueue := deps.Queues.WordResearch

	mux := http.NewServeMux()

	// Realtime + API first (exact patterns win over the "/" catch-all).
	wsHandler := transport.NewHandler(pipe, ident, st, audio, recordings, words, wordVerifyQueue)
	mux.Handle("/ws", wsHandler)
	// The STT ensemble is fixed once pipe is constructed, so its name list is
	// computed once here rather than per health-check request.
	sttNames := pipe.STTNames()
	mux.HandleFunc("/api/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"ok":   true,
			"env":  cfg.Env,
			"stt":  sttNames,
			"chat": cfg.LLMChatModel,
		})
	})
	mux.HandleFunc("/api/me", meHandler(cfg.IdentityMode, ident))
	mux.HandleFunc("GET /api/sessions", sessionsListHandler(ident, st))
	mux.HandleFunc("GET /api/instant-sessions", instantSessionsListHandler(ident, st))
	mux.HandleFunc("POST /api/sessions/{id}/instant", sessionMarkInstantHandler(ident, st))
	mux.HandleFunc("GET /api/sessions/{id}", sessionDetailHandler(ident, st, translateQueue, correctionQueue, pipe, studySummaryQueue, studyQuizQueue))
	mux.HandleFunc("GET /api/sessions/{id}/compaction", sessionCompactionHandler(ident, st))
	mux.HandleFunc("GET /api/sessions/{id}/messages/{turn}/audio", messageAudioHandler(ident, st, articleAudio))
	mux.HandleFunc("POST /api/sessions/{id}/corrections/{turn}/read", sessionCorrectionReadHandler(ident, st))
	mux.HandleFunc("POST /api/sessions/{id}/end", sessionEndHandler(ident, st, pipe, studySummaryQueue, studyQuizQueue))
	mux.HandleFunc("POST /api/sessions/{id}/restudy", sessionRestudyHandler(ident, st, pipe, studySummaryQueue))
	mux.HandleFunc("POST /api/sessions/{id}/quiz/complete", sessionQuizCompleteHandler(ident, st))
	mux.HandleFunc("POST /api/sessions/{id}/quiz/reset", sessionQuizResetHandler(ident, st, pipe, studyQuizQueue))
	var answerCache wordreview.AnswerCache
	if candidate, ok := words.(wordreview.AnswerCache); ok {
		answerCache = candidate
	}
	mux.HandleFunc("POST /api/quiz/check-answer", quizAnswerCheckHandler(ident, pipe, answerCache))
	mux.HandleFunc("DELETE /api/sessions/{id}", sessionDeleteHandler(ident, st, audio, recordings, pipe, profileRegenerateQueue))
	mux.HandleFunc("GET /api/settings", settingsGetHandler(ident, st))
	mux.HandleFunc("PUT /api/settings", settingsSaveHandler(ident, st))
	mux.HandleFunc("GET /api/writing", writingListHandler(ident, writingStore))
	mux.HandleFunc("POST /api/writing/draw", writingDrawHandler(ident, writingStore, st.GetLearnerProfile, pipe, writingQueue))
	mux.HandleFunc("GET /api/writing/{id}", writingInstanceHandler(ident, writingStore))
	mux.HandleFunc("DELETE /api/writing/{id}", writingDeleteHandler(ident, writingStore))
	mux.HandleFunc("POST /api/writing/check", writingCheckHandler(ident, pipe))
	mux.HandleFunc("POST /api/words/suggest", wordSuggestHandler(ident, pipe))
	mux.HandleFunc("POST /api/words/define", wordDefineHandler(ident, pipe))
	mux.HandleFunc("POST /api/words/save", wordSaveHandler(ident, words, pipe, wordVerifyQueue))
	mux.HandleFunc("POST /api/words/auto-add", wordAutoAddHandler(ident, words, st, pipe, wordVerifyQueue, wordAutoAddQueue))
	mux.HandleFunc("GET /api/words/auto-add", wordAutoAddStatusHandler(ident, st))
	mux.HandleFunc("GET /api/words", wordsListHandler(ident, words, pipe, wordVerifyQueue))
	mux.HandleFunc("POST /api/words/{id}/review", wordReviewHandler(ident, words))
	mux.HandleFunc("DELETE /api/words/{id}", wordDeleteHandler(ident, words))
	mux.HandleFunc("POST /api/words/{id}/research", wordResearchHandler(ident, words, pipe, wordResearchQueue))
	mux.HandleFunc("POST /api/words/{id}/research/confirm", wordResearchConfirmHandler(ident, words))
	mux.HandleFunc("GET /api/recordings", recordingsListHandler(ident, recordings))
	mux.HandleFunc("GET /api/recordings/{id}/audio", recordingAudioHandler(ident, recordings))
	mux.HandleFunc("DELETE /api/recordings/{id}", recordingDeleteHandler(ident, audio, recordings))
	mux.HandleFunc("GET /api/articles", articleInstancesListHandler(ident, articles))
	mux.HandleFunc("POST /api/articles/draw", articleDrawHandler(ident, articles, pipe, newsfeed.FetchCandidates, articleStudyQueue, articleAudio))
	mux.HandleFunc("GET /api/articles/{id}", articleInstanceHandler(ident, articles, pipe, articleStudyQueue, articleAudio))
	mux.HandleFunc("GET /api/articles/{id}/audio", articleAudioHandler(ident, articles, articleAudio))
	mux.HandleFunc("POST /api/articles/{id}/answer", articleAnswerHandler(ident, articles))
	mux.HandleFunc("POST /api/articles/{id}/words/define", articleWordDefineHandler(ident, articles, pipe, rdb, wordDefineQueue))
	mux.HandleFunc("DELETE /api/articles/{id}", articleDeleteHandler(ident, articles))
	registerStalePRRedirect(mux, cfg.RootPath)

	// Frontend.
	switch {
	case cfg.IsDev():
		proxy, err := viteProxy(cfg.ViteURL)
		if err != nil {
			log.Fatalf("vite proxy: %v", err)
		}
		mux.Handle("/", proxy)
		log.Printf("dev: proxying frontend -> %s", cfg.ViteURL)
	case assets != nil:
		mux.Handle("/", spaHandlerFS(assets))
		log.Printf("prod: serving embedded frontend")
	default:
		mux.Handle("/", spaHandlerFS(os.DirFS(cfg.WebDist)))
		log.Printf("prod: serving frontend <- %s", cfg.WebDist)
	}

	return &http.Server{
		Addr:              cfg.Addr,
		Handler:           logging(withRootPath(cfg.RootPath, mux)),
		ReadHeaderTimeout: 10 * time.Second,
		// No WriteTimeout: WebSocket connections are long-lived.
	}
}

// withRootPath mounts h under a path prefix (e.g. "/pr/14"), for PR-preview
// deployments where an external router sends /pr/14/* to this instance based
// on URL path rather than a routing header. h keeps registering its patterns
// ("/ws", "/api/health", "/") as if it owned the root; StripPrefix removes
// the prefix before h ever sees the request. A request for the bare prefix
// (no trailing slash) redirects to add one, since the frontend resolves its
// asset/WS URLs relative to the page URL and needs the prefix to look like a
// directory. Requests outside the prefix 404 — this instance only serves
// that one path. rootPath == "" (default) mounts h at "/", unchanged.
func withRootPath(rootPath string, h http.Handler) http.Handler {
	if rootPath == "" {
		return h
	}
	root := http.NewServeMux()
	root.Handle(rootPath+"/", http.StripPrefix(rootPath, h))
	root.HandleFunc(rootPath, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, rootPath+"/", http.StatusMovedPermanently)
	})
	return root
}

// registerStalePRRedirect sends a stray /pr/<n>/* request to home instead of
// letting it fall through to the SPA catch-all. This happens when a
// PR-preview deployment (ROOT_PATH=/pr/14) is torn down but an external
// router keeps forwarding /pr/14/* to the root deployment for lack of
// anywhere else to send it: the SPA catch-all would serve index.html for
// that path too, but its asset URLs are relative and resolve against a path
// (/pr/) the build never targeted, so the JS 404s and the page is blank.
// Only registered on the root deployment itself (rootPath == ""); a
// PR-preview instance never sees an un-prefixed /pr/ path, since
// withRootPath strips its own prefix before this mux sees the request.
func registerStalePRRedirect(mux *http.ServeMux, rootPath string) {
	if rootPath != "" {
		return
	}
	mux.Handle("/pr/", http.RedirectHandler("/", http.StatusFound))
}

func logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		if r.URL.Path != "/ws" { // ws is long-lived; don't log duration noise
			log.Printf("%s %s %s", r.Method, r.URL.Path, time.Since(start).Round(time.Millisecond))
		}
	})
}
