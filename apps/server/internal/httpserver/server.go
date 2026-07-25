// Package httpserver wires the single HTTP entry point: WebSocket, JSON API,
// and the frontend (proxied to Vite in dev, served from dist in prod).
package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"buddy/server/internal/asyncjob"
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
// doc comment for why. translateQueue/correctionQueue are nil when Redis
// isn't configured (see config.Config's RedisClusterHost) —
// sessionDetailHandler simply stops queueing translation/correction
// backfills, the same "optional feature, falls through to doing nothing"
// convention as audio/recordings above. titleQueue is the same kind of
// optional wiring for durable title generation — see
// transport.Handler.SetTitleQueue.
func New(cfg config.Config, pipe *pipeline.Pipeline, assets fs.FS, ident identity.Identifier, st store.Store, audio transport.AudioSaver, recordings recording.Store, translateQueue *backfill.Queue, correctionQueue *backfill.CorrectionQueue, titleQueue *asyncjob.Queue) *http.Server {
	mux := http.NewServeMux()

	// Realtime + API first (exact patterns win over the "/" catch-all).
	wsHandler := transport.NewHandler(pipe, ident, st, audio, recordings)
	wsHandler.SetTitleQueue(titleQueue)
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
	mux.HandleFunc("GET /api/sessions/{id}", sessionDetailHandler(ident, st, translateQueue, correctionQueue))
	mux.HandleFunc("GET /api/sessions/{id}/compaction", sessionCompactionHandler(ident, st))
	mux.HandleFunc("DELETE /api/sessions/{id}", sessionDeleteHandler(ident, st, audio, recordings))
	mux.HandleFunc("GET /api/settings", settingsGetHandler(ident, st))
	mux.HandleFunc("PUT /api/settings", settingsSaveHandler(ident, st))
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

// defaultSessionPageLimit is how many distinct turns sessionDetailHandler
// loads when the request doesn't specify ?limit= — enough for most rooms to
// load in one page, small enough that a very long-running room's replay
// doesn't ship its entire history (and every embedded correction/
// translation) on first paint. The frontend requests older pages by turn
// cursor (see ?before=) as the learner scrolls up.
const defaultSessionPageLimit = 30

// sessionDetailHandler returns a page of one session's transcript for
// replay, most recent turns first: ?before= (a turn number cursor — omit or
// 0 for the latest page) and ?limit= (page size — omit or non-positive for
// defaultSessionPageLimit) select the page, and the "hasMore" field in the
// response reports whether older turns exist beyond it. An explicit
// non-positive ?limit= instead requests the whole transcript via the
// unbounded store.SessionDetail, with "hasMore" always false. Either way,
// the store scopes the lookup by the caller's own userID, so a session ID
// belonging to someone else 404s exactly like one that doesn't exist at
// all — this handler can't tell the difference, on purpose.
//
// Viewing a session is also what triggers translation and correction
// backfill: if any turn on the returned page is missing its native-language
// translation (saved before the translation feature existed, or a one-off
// async failure at the time — see internal/backfill's doc comment), the
// session is queued for background re-translation; likewise, if any user
// turn is missing a grammar-correction result entirely — CorrectionStatus ==
// "" (see store.Turn's doc comment) — it's queued for background
// re-correction via asyncjob.KindCorrectionBackfill, since (unlike a
// CorrectionStatus == "failed" turn) nothing else would ever retry it. Either
// way this never delays the response: Enqueue is a couple of fast Redis
// calls, but it's still fired via `go` so a slow/unavailable Redis can never
// make opening a conversation wait on it, and the actual work happens
// entirely out-of-band in internal/backfill's Workers, over the session's
// whole transcript regardless of which page triggered it — the learner sees
// today's (possibly still-missing) results immediately and gets the
// filled-in ones on their next visit.
func sessionDetailHandler(ident identity.Identifier, st store.Store, translateQueue *backfill.Queue, correctionQueue *backfill.CorrectionQueue) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, ok := requireUser(w, r, ident)
		if !ok {
			return
		}
		sessionID := r.PathValue("id")
		beforeTurn, _ := strconv.Atoi(r.URL.Query().Get("before"))
		// limit is only defaulted when the caller omits it entirely (or sends
		// something unparseable) — an explicit "limit=0" (or negative) is a
		// deliberate request for the whole transcript (routed to the
		// unbounded store.SessionDetail below), which is how
		// pollMissingFeedback (apps/web/src/App.tsx) still polls every turn
		// rather than just the latest page.
		limit := defaultSessionPageLimit
		if v := r.URL.Query().Get("limit"); v != "" {
			if n, err := strconv.Atoi(v); err == nil {
				limit = n
			}
		}
		var meta store.SessionMeta
		var turns []store.Turn
		var hasMore bool
		var err error
		if limit <= 0 {
			meta, turns, err = st.SessionDetail(r.Context(), userID, sessionID)
		} else {
			meta, turns, hasMore, err = st.SessionDetailPage(r.Context(), userID, sessionID, beforeTurn, limit)
		}
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
		if needsCorrectionBackfill(turns) {
			go correctionQueue.Enqueue(context.Background(), userID, sessionID)
		}
		writeJSON(w, map[string]any{"session": meta, "turns": turns, "hasMore": hasMore})
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

// needsCorrectionBackfill reports whether any non-blank user turn in the
// transcript is missing a grammar-correction result entirely —
// CorrectionStatus == "" and no Correction yet. A turn with CorrectionStatus
// "pending"/"processing"/"failed" is excluded on purpose: those are already
// tracked by a live asyncjob.KindCorrection job, whose own reaper retries a
// failed attempt on its own (see pollMissingFeedback in apps/web/src/App.tsx)
// — only a turn no job was ever reserved for needs this backfill path.
func needsCorrectionBackfill(turns []store.Turn) bool {
	for _, t := range turns {
		if t.Role == "user" && strings.TrimSpace(t.Text) != "" && t.Correction == nil && t.CorrectionStatus == "" {
			return true
		}
	}
	return false
}

// sessionCompactionHandler exposes a session's current LLM-context state —
// the rolling summary plus how many verbatim turns still sit in the
// uncompacted window — for the learner to inspect, and how many turns exist
// in the session overall (store.LastTurn — see internal/session's doc for how
// the two relate: the summary is what got folded away, the window is what's
// still sent to the model verbatim). This is purely informational: nothing
// here is ever dropped from store.Turn's own full transcript (see
// sessionDetailHandler), only from the copy of the conversation sent to the
// LLM.
func sessionCompactionHandler(ident identity.Identifier, st store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, ok := requireUser(w, r, ident)
		if !ok {
			return
		}
		sessionID := r.PathValue("id")

		// Load and LastTurn are independent reads (one from the replica, one
		// from the primary — see LastTurn's doc), so run them concurrently
		// rather than paying two sequential round trips.
		var profile store.Profile
		var lastTurn int
		var loadErr, lastTurnErr error
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			profile, loadErr = st.Load(r.Context(), userID, sessionID)
		}()
		go func() {
			defer wg.Done()
			lastTurn, lastTurnErr = st.LastTurn(r.Context(), userID, sessionID)
		}()
		wg.Wait()
		if loadErr != nil {
			serverError(w, "load profile", loadErr)
			return
		}
		if lastTurnErr != nil {
			serverError(w, "last turn", lastTurnErr)
			return
		}
		writeJSON(w, map[string]any{
			"summary":        profile.Summary,
			"recentMessages": len(profile.Recent),
			"totalTurns":     lastTurn,
		})
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
		userID, ok := requireUser(w, r, ident)
		if !ok {
			return
		}
		sessionID := r.PathValue("id")
		var wg sync.WaitGroup
		if audio != nil {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := audio.DeleteBySession(r.Context(), userID, sessionID); err != nil {
					log.Printf("delete session: cascade audio backups %s/%s: %v", userID, sessionID, err)
				}
			}()
		}
		if recordings != nil {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := recordings.DeleteBySession(r.Context(), userID, sessionID); err != nil {
					log.Printf("delete session: cascade recordings %s/%s: %v", userID, sessionID, err)
				}
			}()
		}
		wg.Wait()
		if err := st.DeleteSession(r.Context(), userID, sessionID); err != nil {
			serverError(w, "delete session", err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// maxInterlocutorStyleLen bounds the free-text conversation-style preference
// so a learner can't balloon every chat session's system prompt (and LLM
// cost) with an arbitrarily long paste.
const maxInterlocutorStyleLen = 500

// settingsGetHandler returns the caller's own saved conversation-style
// preference — personal, not admin, like sessionsListHandler: scoped to
// whatever ident.Identify resolves to.
func settingsGetHandler(ident identity.Identifier, st store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, ok := requireUser(w, r, ident)
		if !ok {
			return
		}
		style, err := st.GetInterlocutorStyle(r.Context(), userID)
		if err != nil {
			serverError(w, "get interlocutor style", err)
			return
		}
		writeJSON(w, map[string]any{"interlocutorStyle": style})
	}
}

// settingsSaveHandler saves the caller's conversation-style preference. It
// takes effect on the chat persona of sessions created from now on (see
// pipeline.BuildSystemPrompt) — an already-open WS connection keeps the
// system prompt it started with, since transport.Handler.ServeHTTP only
// reads this once, at session creation.
func settingsSaveHandler(ident identity.Identifier, st store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, ok := requireUser(w, r, ident)
		if !ok {
			return
		}
		var body struct {
			InterlocutorStyle string `json:"interlocutorStyle"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		style := strings.TrimSpace(body.InterlocutorStyle)
		if utf8.RuneCountInString(style) > maxInterlocutorStyleLen {
			http.Error(w, fmt.Sprintf("interlocutorStyle exceeds %d characters", maxInterlocutorStyleLen), http.StatusBadRequest)
			return
		}
		if err := st.SaveInterlocutorStyle(r.Context(), userID, style); err != nil {
			serverError(w, "save interlocutor style", err)
			return
		}
		writeJSON(w, map[string]any{"interlocutorStyle": style})
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

// requireRecordings guards the recording-endpoint handlers against a nil
// Store (archival disabled), writing a 503 and reporting false on failure so
// callers can `if !requireRecordings(w, recordings) { return }`.
func requireRecordings(w http.ResponseWriter, recordings recording.Store) bool {
	if recordings == nil {
		http.Error(w, "recording storage is not configured", http.StatusServiceUnavailable)
		return false
	}
	return true
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
