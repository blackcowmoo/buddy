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
	"buddy/server/internal/pipeline"
	"buddy/server/internal/recording"
	"buddy/server/internal/store"
	"buddy/server/internal/transport"
	"buddy/server/internal/wordreview"
)

// New builds the single HTTP entry point. assets is the embedded frontend FS
// (from webassets.FS()); pass nil to serve from disk (prod) or proxy (dev).
// audio is optional (nil disables temporary S3 audio backup — see
// internal/audiostore). recordings is nil when voice-recording archival is
// disabled (see config.Config's S3Bucket) — the /api/recordings routes still
// exist but answer 503. audio and recordings are two independent features
// that happen to share the same S3_* config — see internal/recording.S3Config's
// doc comment for why. translateQueue/correctionQueue are nil when Redis
// isn't configured (see config.Config's RedisClusterHost) —
// sessionDetailHandler simply stops queueing translation/correction
// backfills, the same "optional feature, falls through to doing nothing"
// convention as audio/recordings above. Durable title generation is wired
// the same optional way, but directly onto pipe.TitleHook by the caller
// (see cmd/server/main.go) rather than through a parameter here — same as
// pipe.ReplyHook/CorrectHook/TranslateHook. studySummaryQueue/studyQuizQueue
// are nil the same optional way — sessionEndHandler falls back to running
// the wrap-up/quiz inline, each on its own detached goroutine, instead of
// durably queuing them (see transport.EnqueueStudySummaryJob/
// EnqueueStudyQuizJob). words is never nil — unlike recordings, the
// word-review study list (internal/wordreview) has no optional external
// dependency, so cmd/server/main.go always constructs it. wordVerifyQueue is
// nil the same optional way as studySummaryQueue/studyQuizQueue —
// wordSaveHandler falls back to running the model-consensus check inline on
// its own detached goroutine instead of durably queuing it (see
// transport.EnqueueWordVerifyJob).
func New(cfg config.Config, pipe *pipeline.Pipeline, assets fs.FS, ident identity.Identifier, st store.Store, audio transport.AudioSaver, recordings recording.Store, words wordreview.Store, wordVerifyQueue *asyncjob.Queue, translateQueue *backfill.Queue, correctionQueue *backfill.CorrectionQueue, studySummaryQueue *asyncjob.Queue, studyQuizQueue *asyncjob.Queue) *http.Server {
	mux := http.NewServeMux()

	// Realtime + API first (exact patterns win over the "/" catch-all).
	wsHandler := transport.NewHandler(pipe, ident, st, audio, recordings)
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
	mux.HandleFunc("GET /api/sessions/{id}", sessionDetailHandler(ident, st, translateQueue, correctionQueue, pipe, studySummaryQueue, studyQuizQueue))
	mux.HandleFunc("GET /api/sessions/{id}/compaction", sessionCompactionHandler(ident, st))
	mux.HandleFunc("POST /api/sessions/{id}/end", sessionEndHandler(ident, st, pipe, studySummaryQueue, studyQuizQueue))
	mux.HandleFunc("POST /api/sessions/{id}/restudy", sessionRestudyHandler(ident, st, pipe, studySummaryQueue))
	mux.HandleFunc("POST /api/sessions/{id}/quiz/complete", sessionQuizCompleteHandler(ident, st))
	mux.HandleFunc("POST /api/sessions/{id}/quiz/reset", sessionQuizResetHandler(ident, st, pipe, studyQuizQueue))
	mux.HandleFunc("POST /api/quiz/check-answer", quizAnswerCheckHandler(ident, pipe))
	mux.HandleFunc("DELETE /api/sessions/{id}", sessionDeleteHandler(ident, st, audio, recordings))
	mux.HandleFunc("GET /api/settings", settingsGetHandler(ident, st))
	mux.HandleFunc("PUT /api/settings", settingsSaveHandler(ident, st))
	mux.HandleFunc("POST /api/words/suggest", wordSuggestHandler(ident, pipe))
	mux.HandleFunc("POST /api/words/save", wordSaveHandler(ident, words, pipe, wordVerifyQueue))
	mux.HandleFunc("GET /api/words", wordsListHandler(ident, words))
	mux.HandleFunc("POST /api/words/{id}/review", wordReviewHandler(ident, words))
	mux.HandleFunc("DELETE /api/words/{id}", wordDeleteHandler(ident, words))
	mux.HandleFunc("GET /api/recordings", recordingsListHandler(ident, recordings))
	mux.HandleFunc("GET /api/recordings/{id}/audio", recordingAudioHandler(ident, recordings))
	mux.HandleFunc("DELETE /api/recordings/{id}", recordingDeleteHandler(ident, audio, recordings))
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
