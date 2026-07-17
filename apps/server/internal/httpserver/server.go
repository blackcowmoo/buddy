// Package httpserver wires the single HTTP entry point: WebSocket, JSON API,
// and the frontend (proxied to Vite in dev, served from dist in prod).
package httpserver

import (
	"encoding/json"
	"errors"
	"io/fs"
	"log"
	"net/http"
	"os"
	"time"

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
// doc comment for why.
func New(cfg config.Config, pipe *pipeline.Pipeline, assets fs.FS, ident identity.Identifier, st store.Store, audio transport.AudioSaver, recordings recording.Store) *http.Server {
	mux := http.NewServeMux()

	// Realtime + API first (exact patterns win over the "/" catch-all).
	mux.Handle("/ws", transport.NewHandler(pipe, ident, st, audio, recordings))
	mux.HandleFunc("/api/health", func(w http.ResponseWriter, r *http.Request) {
		sttNames := make([]string, len(pipe.STT))
		for i, rec := range pipe.STT {
			sttNames[i] = rec.Name()
		}
		writeJSON(w, map[string]any{
			"ok":   true,
			"env":  cfg.Env,
			"stt":  sttNames,
			"chat": cfg.LLMChatModel,
		})
	})
	mux.HandleFunc("/api/me", meHandler(cfg.IdentityMode, ident))
	mux.HandleFunc("GET /api/sessions", sessionsListHandler(ident, st))
	mux.HandleFunc("GET /api/sessions/{id}", sessionDetailHandler(ident, st))
	mux.HandleFunc("DELETE /api/sessions/{id}", sessionDeleteHandler(ident, st, audio, recordings))
	mux.HandleFunc("GET /api/recordings", recordingsListHandler(ident, recordings))
	mux.HandleFunc("GET /api/recordings/{id}/audio", recordingAudioHandler(ident, recordings))
	mux.HandleFunc("DELETE /api/recordings/{id}", recordingDeleteHandler(ident, recordings))
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
		userID, ok := ident.Identify(w, r)
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
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
		userID, ok := ident.Identify(w, r)
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		sessions, err := st.ListSessions(r.Context(), userID)
		if err != nil {
			log.Printf("list sessions: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		writeJSON(w, sessions)
	}
}

// sessionDetailHandler returns one session's full transcript for replay.
// store.SessionDetail scopes the lookup by the caller's own userID, so a
// session ID belonging to someone else 404s exactly like one that doesn't
// exist at all — this handler can't tell the difference, on purpose.
func sessionDetailHandler(ident identity.Identifier, st store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, ok := ident.Identify(w, r)
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		meta, turns, err := st.SessionDetail(r.Context(), userID, r.PathValue("id"))
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if err != nil {
			log.Printf("session detail: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]any{"session": meta, "turns": turns})
	}
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
		userID, ok := ident.Identify(w, r)
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
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
			log.Printf("delete session: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
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
