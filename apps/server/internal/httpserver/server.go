// Package httpserver wires the single HTTP entry point: WebSocket, JSON API,
// and the frontend (proxied to Vite in dev, served from dist in prod).
package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"buddy/server/internal/backfill"
	"buddy/server/internal/config"
	"buddy/server/internal/identity"
	"buddy/server/internal/pipeline"
	"buddy/server/internal/recording"
	"buddy/server/internal/store"
	"buddy/server/internal/transport"
)

// New builds the single HTTP entry point. assets is the embedded frontend FS
// (from webassets.FS()); pass nil to serve from disk (prod) or proxy (dev).
// audio is optional (nil disables temporary S3 audio backup — see
// internal/audiostore). recordings is nil when voice-recording archival is
// disabled (see config.Config's S3Bucket) — the /api/recordings routes still
// exist but answer 503. audio and recordings are two independent features
// that happen to share the same S3_* config — see internal/recording.S3Config's
// doc comment for why. translateQueue is nil when Redis isn't configured (see
// config.Config's RedisClusterHost) — sessionDetailHandler simply stops
// queueing translation backfills, the same "optional feature, falls through
// to doing nothing" convention as audio/recordings above.
func New(cfg config.Config, pipe *pipeline.Pipeline, assets fs.FS, ident identity.Identifier, st store.Store, audio transport.AudioSaver, recordings recording.Store, translateQueue *backfill.Queue) *http.Server {
	mux := http.NewServeMux()

	// Realtime + API first (exact patterns win over the "/" catch-all).
	mux.Handle("/ws", transport.NewHandler(pipe, ident, st, audio, recordings))
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
	mux.HandleFunc("GET /api/sessions/{id}", sessionDetailHandler(ident, st, translateQueue))
	mux.HandleFunc("DELETE /api/sessions/{id}", sessionDeleteHandler(ident, st, audio, recordings))
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

// meHandler resolves the caller's identity the same way the /ws handshake
// does, so the frontend can show something better than an opaque cookie
// value in its menu (see apps/web/src/lib/me.ts): the OIDC "email" claim
// when identityMode is "oidc", and nothing meaningful otherwise — the
// CookieIdentifier's id is a random per-browser token, not a real identity,
// so the frontend only trusts it when identityMode says it's real.
func meHandler(identityMode string, ident identity.Identifier) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, ok := requireUser(w, r, ident)
		if !ok {
			return
		}
		writeJSON(w, map[string]any{
			"identityMode": identityMode,
			"id":           userID,
		})
	}
}

// sessionsListHandler returns the caller's own chat rooms — personal, not
// admin: scoped to whatever ident.Identify resolves to, the same identity
// /ws and /api/me use, so a learner only ever sees their own history.
func sessionsListHandler(ident identity.Identifier, st store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, ok := requireUser(w, r, ident)
		if !ok {
			return
		}
		sessions, err := st.ListSessions(r.Context(), userID)
		if err != nil {
			serverError(w, "list sessions", err)
			return
		}
		writeJSON(w, sessions)
	}
}

// sessionDetailHandler returns one session's full transcript for replay.
// store.SessionDetail scopes the lookup by the caller's own userID, so a
// session ID belonging to someone else 404s exactly like one that doesn't
// exist at all — this handler can't tell the difference, on purpose.
//
// Viewing a session is also what triggers translation backfill: if any turn
// is missing its native-language translation (saved before the translation
// feature existed, or a one-off async failure at the time — see
// internal/backfill's doc comment), the session is queued for background
// re-translation. This never delays the response: Enqueue is a couple of
// fast Redis calls, but it's still fired via `go` so a slow/unavailable
// Redis can never make opening a conversation wait on it, and the actual
// translation work happens entirely out-of-band in internal/backfill.Worker
// — the learner sees today's (possibly still-missing) translations
// immediately and gets the filled-in ones on their next visit.
func sessionDetailHandler(ident identity.Identifier, st store.Store, translateQueue *backfill.Queue) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, ok := requireUser(w, r, ident)
		if !ok {
			return
		}
		sessionID := r.PathValue("id")
		meta, turns, err := st.SessionDetail(r.Context(), userID, sessionID)
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if err != nil {
			serverError(w, "session detail", err)
			return
		}
		if needsTranslationBackfill(turns) {
			go translateQueue.Enqueue(context.Background(), userID, sessionID)
		}
		writeJSON(w, map[string]any{"session": meta, "turns": turns})
	}
}

// needsTranslationBackfill reports whether any non-blank turn in the
// transcript is missing its native-language translation.
func needsTranslationBackfill(turns []store.Turn) bool {
	for _, t := range turns {
		if strings.TrimSpace(t.Text) != "" && strings.TrimSpace(t.Translation) == "" {
			return true
		}
	}
	return false
}

// sessionDeleteHandler removes one chat room and its full transcript, and
// cascades to that room's audio: every recording captured in it (see
// recording.Store.DeleteBySession, when archival is enabled) and every
// temporary backup of its raw utterance audio (see
// transport.AudioSaver.DeleteBySession, when backup is enabled) — these are
// two independent S3-backed features (see internal/recording's package doc),
// so both cascades run regardless of which one, if either, is configured.
// Deleting a conversation must not leave orphaned audio of either kind
// behind. Both cascades are best-effort: a failure is logged but doesn't
// block deleting the session itself, the same "side-effect independent of
// the primary action" pattern transport.Handler.backupAudio uses for its own
// S3 writes.
func sessionDeleteHandler(ident identity.Identifier, st store.Store, audio transport.AudioSaver, recordings recording.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, ok := requireUser(w, r, ident)
		if !ok {
			return
		}
		sessionID := r.PathValue("id")
		if audio != nil {
			if err := audio.DeleteBySession(r.Context(), userID, sessionID); err != nil {
				log.Printf("delete session: cascade audio backups %s/%s: %v", userID, sessionID, err)
			}
		}
		if recordings != nil {
			if err := recordings.DeleteBySession(r.Context(), userID, sessionID); err != nil {
				log.Printf("delete session: cascade recordings %s/%s: %v", userID, sessionID, err)
			}
		}
		if err := st.DeleteSession(r.Context(), userID, sessionID); err != nil {
			serverError(w, "delete session", err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// requireUser resolves the caller's identity the way every JSON-API handler
// needs to, writing a 401 and reporting false on failure so callers can
// `userID, ok := requireUser(...); if !ok { return }`.
func requireUser(w http.ResponseWriter, r *http.Request, ident identity.Identifier) (string, bool) {
	userID, ok := ident.Identify(w, r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return "", false
	}
	return userID, true
}

// serverError logs err with context and writes a generic 500 — the response
// body never leaks internal error detail to the caller.
func serverError(w http.ResponseWriter, context string, err error) {
	log.Printf("%s: %v", context, err)
	http.Error(w, "internal error", http.StatusInternalServerError)
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
