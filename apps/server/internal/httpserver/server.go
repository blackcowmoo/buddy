// Package httpserver wires the single HTTP entry point: WebSocket, JSON API,
// and the frontend (proxied to Vite in dev, served from dist in prod).
package httpserver

import (
	"encoding/json"
	"io/fs"
	"log"
	"net/http"
	"os"
	"time"

	"buddy/server/internal/config"
	"buddy/server/internal/identity"
	"buddy/server/internal/pipeline"
	"buddy/server/internal/store"
	"buddy/server/internal/transport"
)

// New builds the single HTTP entry point. assets is the embedded frontend FS
// (from webassets.FS()); pass nil to serve from disk (prod) or proxy (dev).
func New(cfg config.Config, pipe *pipeline.Pipeline, assets fs.FS, ident identity.Identifier, st store.Store) *http.Server {
	mux := http.NewServeMux()

	// Realtime + API first (exact patterns win over the "/" catch-all).
	mux.Handle("/ws", transport.NewHandler(pipe, ident, st))
	mux.HandleFunc("/api/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"ok":       true,
			"env":      cfg.Env,
			"fast_stt": pipe.FastSTT.Name(),
			"slow_stt": pipe.SlowSTT.Name(),
			"chat":     cfg.LLMChatModel,
		})
	})
	mux.HandleFunc("/api/me", meHandler(cfg.IdentityMode, ident))

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
