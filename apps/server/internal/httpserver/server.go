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
// CorrectionStatus == "failed" turn) nothing else would ever retry it. The
// same goes for a study-summary wrap-up left in JobStatusPending with no
// StudySummary yet — see needsStudySummaryBackfill — which is how the
// legacy-reset migration in store.NewMySQL gets its rows regenerated rather
// than left permanently blank, and for a session ended before quiz
// pre-generation existed at all — see needsStudyQuizBackfill, which
// re-enqueues asyncjob.KindStudyQuiz the first time such a session is
// reopened, so it only ever needs generating once rather than staying stuck
// on the old on-demand-at-click-time path forever. Either way this never
// delays the response: Enqueue is a couple of fast Redis calls, but it's
// still fired via `go` so a slow/unavailable Redis can never make opening a
// conversation wait on it, and the actual work happens entirely out-of-band
// in internal/backfill's Workers (or the pooled asyncjob.KindStudySummary/
// KindStudyQuiz Workers — see cmd/server/main.go), over the session's whole
// transcript regardless of which page triggered it — the learner sees
// today's (possibly still-missing) results immediately and gets the
// filled-in ones on their next visit.
func sessionDetailHandler(ident identity.Identifier, st store.Store, translateQueue *backfill.Queue, correctionQueue *backfill.CorrectionQueue, pipe *pipeline.Pipeline, studySummaryQueue *asyncjob.Queue, studyQuizQueue *asyncjob.Queue) http.HandlerFunc {
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
		if needsStudySummaryBackfill(meta, turns) {
			go func() {
				if err := transport.EnqueueStudySummaryJob(context.Background(), studySummaryQueue, pipe, st, userID, sessionID); err != nil {
					log.Printf("session detail: enqueue study summary backfill %s/%s: %v", userID, sessionID, err)
				}
			}()
		}
		if needsStudyQuizBackfill(meta) {
			go func() {
				if err := transport.EnqueueStudyQuizJob(context.Background(), studyQuizQueue, pipe, st, userID, sessionID); err != nil {
					log.Printf("session detail: enqueue study quiz backfill %s/%s: %v", userID, sessionID, err)
				}
			}()
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

// needsStudySummaryBackfill reports whether an ended session's wrap-up needs
// (re)generating: StudySummaryStatus == JobStatusPending with no
// StudySummary yet, but real flagged issues still sitting in the transcript's
// per-turn corrections. That combination only arises from the legacy-reset
// migration in store.NewMySQL, which resets a pre-bilingual "done" wrap-up
// back to pending rather than leaving it stuck — a freshly-ended session
// that's still actually being generated has the exact same status, but
// hasn't had a chance to accumulate a transcript worth flagging issues in
// yet, so gating on CollectStudyIssues here doesn't fight the live job (and
// even if it did, EnqueueStudySummaryJob's dedupe makes a redundant enqueue
// harmless). A session with genuinely nothing to flag never reaches this
// check: len(issues) == 0 short-circuits it.
func needsStudySummaryBackfill(meta store.SessionMeta, turns []store.Turn) bool {
	if !meta.Ended || meta.StudySummaryStatus != store.JobStatusPending || len(meta.StudySummary) != 0 {
		return false
	}
	return len(transport.CollectStudyIssues(turns)) > 0
}

// needsStudyQuizBackfill reports whether an ended session predates quiz
// pre-generation entirely: QuizStatus == "" only ever arises from a session
// that was ended before asyncjob.KindStudyQuiz existed (see EndSession,
// which now sets QuizStatus to JobStatusPending the instant it ends any
// session) — every session ended since then reaches a terminal QuizStatus
// (JobStatusDone, even with an empty quiz — see runStudyQuiz) on its own, so
// there's no live job here for a redundant enqueue to race, unlike
// needsStudySummaryBackfill's legacy-reset case. Doesn't gate on
// CollectStudyIssues the way needsStudySummaryBackfill does: an ended
// session with no flagged issues still needs its QuizStatus moved off ""
// (to JobStatusDone with an empty quiz), or it would look "still pending"
// forever.
func needsStudyQuizBackfill(meta store.SessionMeta) bool {
	return meta.Ended && meta.QuizStatus == ""
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

// enqueueOrRunInline is the shared "durable queue if Redis is configured,
// otherwise a detached best-effort goroutine" fallback behind
// sessionEndHandler's and sessionRestudyHandler's study-summary/quiz kickoff:
// when queue is non-nil, enqueue runs synchronously on ctx (cheap — no LLM
// call — the queue's own EnqueueAndRunInBackground handles backgrounding the
// actual work); otherwise inline runs the whole job itself, so it must be
// backgrounded here on a detached context.Background() goroutine to get the
// same "outlives this response" behavior without Redis.
func enqueueOrRunInline(queue *asyncjob.Queue, ctx context.Context, enqueueErrLabel string, enqueue func(ctx context.Context) error, inlineErrLabel string, inline func(ctx context.Context) error) {
	if queue != nil {
		if err := enqueue(ctx); err != nil {
			log.Printf("%s: %v", enqueueErrLabel, err)
		}
		return
	}
	go func() {
		if err := inline(context.Background()); err != nil {
			log.Printf("%s: %v", inlineErrLabel, err)
		}
	}()
}

// sessionEndHandler permanently marks one chat room read-only
// (store.Store.EndSession) the instant the learner confirms "end this
// conversation" (see EndConversationControl in apps/web/src/App.tsx) —
// freezing never waits on either background job's LLM call, which this
// kicks off separately right after (transport.EnqueueStudySummaryJob and
// transport.EnqueueStudyQuizJob, run independently rather than chained), so
// the room is safely read-only, and the response comes back, before either
// call has even started. The wrap-up — and folding it into the learner's
// persistent cross-session profile — happens in the background from there
// (see transport.StudySummaryJobHandler/runStudySummary), and the practice
// quiz alongside it (see transport.StudyQuizJobHandler/runStudyQuiz), pre-
// generated now instead of on demand so the "퀴즈 풀기" button later reads an
// already-finished result: both survive the learner navigating away right
// after this returns, which is the entire point of routing them through
// asyncjob rather than generating them inline in this request.
func sessionEndHandler(ident identity.Identifier, st store.Store, pipe *pipeline.Pipeline, studySummaryQueue *asyncjob.Queue, studyQuizQueue *asyncjob.Queue) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, ok := requireUser(w, r, ident)
		if !ok {
			return
		}
		sessionID := r.PathValue("id")

		if err := st.EndSession(r.Context(), userID, sessionID); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				http.NotFound(w, r)
				return
			}
			serverError(w, "end session", err)
			return
		}

		// Kicked off concurrently, not sequentially: both are independent, and
		// each blocks the response on its own Enqueue round trip (see
		// enqueueOrRunInline), so running them one after another would pay
		// that latency twice for no reason.
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			enqueueOrRunInline(studySummaryQueue, r.Context(),
				fmt.Sprintf("end session: enqueue study summary %s/%s", userID, sessionID),
				func(ctx context.Context) error {
					return transport.EnqueueStudySummaryJob(ctx, studySummaryQueue, pipe, st, userID, sessionID)
				},
				fmt.Sprintf("end session: study summary %s/%s", userID, sessionID),
				func(ctx context.Context) error {
					return transport.RunStudySummaryInline(ctx, pipe, st, userID, sessionID)
				},
			)
		}()
		go func() {
			defer wg.Done()
			enqueueOrRunInline(studyQuizQueue, r.Context(),
				fmt.Sprintf("end session: enqueue study quiz %s/%s", userID, sessionID),
				func(ctx context.Context) error {
					return transport.EnqueueStudyQuizJob(ctx, studyQuizQueue, pipe, st, userID, sessionID)
				},
				fmt.Sprintf("end session: study quiz %s/%s", userID, sessionID),
				func(ctx context.Context) error {
					return transport.RunStudyQuizInline(ctx, pipe, st, userID, sessionID)
				},
			)
		}()
		wg.Wait()

		w.WriteHeader(http.StatusNoContent)
	}
}

// sessionRestudyHandler lets a learner force-regenerate an ended session's
// study-summary wrap-up when it landed as JobStatusDone with an empty
// result — the "다시 확인하기" button EndConversationControl (apps/web/src/
// App.tsx) shows only in that exact state. That combination should be rare
// now that runStudySummary itself treats an empty result as a failure
// (see its doc comment), but a session that already landed there before
// that fix — or hit some other still-unknown gap — has no other way back:
// needsStudySummaryBackfill only re-triggers a JobStatusPending row, and
// this one reads as done. Gated server-side on the same state the button is
// shown for (not just trusting the client) so this can only ever regenerate
// an empty wrap-up, never clobber one that already has real content. A
// session ended before the wrap-up became an async job reads
// StudySummaryStatus as "" rather than JobStatusDone (see the
// store.SessionMeta.StudySummaryStatus doc comment) — the button's own
// gating (and EndConversationControl's rendering) already treats "" the
// same as done, so this check must too, or every one of those legacy
// sessions 409s the instant a learner taps the button.
func sessionRestudyHandler(ident identity.Identifier, st store.Store, pipe *pipeline.Pipeline, studySummaryQueue *asyncjob.Queue) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, ok := requireUser(w, r, ident)
		if !ok {
			return
		}
		sessionID := r.PathValue("id")

		meta, _, err := st.SessionDetail(r.Context(), userID, sessionID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				http.NotFound(w, r)
				return
			}
			serverError(w, "session detail", err)
			return
		}
		done := meta.StudySummaryStatus == store.JobStatusDone || meta.StudySummaryStatus == ""
		if !meta.Ended || !done || len(meta.StudySummary) != 0 {
			http.Error(w, "study summary is not in a re-checkable state", http.StatusConflict)
			return
		}

		if err := st.RestartStudySummary(r.Context(), userID, sessionID); err != nil {
			serverError(w, "restart study summary", err)
			return
		}

		enqueueOrRunInline(studySummaryQueue, r.Context(),
			fmt.Sprintf("restudy session: enqueue study summary %s/%s", userID, sessionID),
			func(ctx context.Context) error {
				return transport.EnqueueStudySummaryJob(ctx, studySummaryQueue, pipe, st, userID, sessionID)
			},
			fmt.Sprintf("restudy session: study summary %s/%s", userID, sessionID),
			func(ctx context.Context) error {
				return transport.RunStudySummaryInline(ctx, pipe, st, userID, sessionID)
			},
		)

		w.WriteHeader(http.StatusNoContent)
	}
}

// sessionQuizCompleteHandler marks an ended session's quiz as studied (see
// SessionMeta.QuizCompleted's doc comment) — called by the frontend either
// once a learner answers every quiz question correctly (grading happens
// entirely client-side in QuizPanel, the same trust-the-client model
// normalizeQuizAnswer already used before this endpoint existed), or, for a
// session whose quiz came back with no questions at all, when the learner
// taps "내가 읽었음" (I've read it) instead — both cases mean the same thing
// for the room list's badge: this session's feedback has been studied.
// Deliberately not gated on server-side state the way sessionRestudyHandler
// is: unlike regenerating a wrap-up (a real, costly LLM call this handler
// must protect from replay), setting one boolean is harmless to call more
// than once or in an unexpected state.
func sessionQuizCompleteHandler(ident identity.Identifier, st store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, ok := requireUser(w, r, ident)
		if !ok {
			return
		}
		sessionID := r.PathValue("id")

		if err := st.MarkQuizCompleted(r.Context(), userID, sessionID); err != nil {
			serverError(w, "mark quiz completed", err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
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
const maxInterlocutorStyleLen = 1024

// settingsGetHandler returns the caller's own saved conversation-style
// preference, alongside their persistent cross-session learner profile (see
// store.Store.GetLearnerProfile) — read-only here, since nothing under
// /api/settings writes it; it's folded in only so the frontend's one
// settings fetch (see fetchSettings/MenuPanel in App.tsx) can show it next
// to the style form instead of a second round trip. Personal, not admin,
// like sessionsListHandler: scoped to whatever ident.Identify resolves to.
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
		profile, err := st.GetLearnerProfile(r.Context(), userID)
		if err != nil {
			serverError(w, "get learner profile", err)
			return
		}
		writeJSON(w, map[string]any{"interlocutorStyle": style, "learnerProfile": profile})
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

// maxWordQueryLen caps the Korean description a learner can send to
// wordSuggestHandler — generous for the "short phrase describing a word"
// framing wordSuggestionSystemPrompt asks for, same reasoning as
// maxInterlocutorStyleLen guarding against an arbitrarily long paste.
const maxWordQueryLen = 200

// wordSuggestHandler asks pipeline.SuggestWords for English word/phrase
// candidates matching a learner's native-language description — a quick,
// synchronous call (unlike GenerateStudySummary/GenerateStudyQuiz's
// asyncjob-backed handlers) since it's cheap, latency-sensitive, and has
// nothing worth persisting durably: a dropped request just gets retried by
// the learner reopening the panel.
func wordSuggestHandler(ident identity.Identifier, pipe *pipeline.Pipeline) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_, ok := requireUser(w, r, ident)
		if !ok {
			return
		}
		var body struct {
			Query string `json:"query"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		query := strings.TrimSpace(body.Query)
		if query == "" {
			http.Error(w, "query is required", http.StatusBadRequest)
			return
		}
		if utf8.RuneCountInString(query) > maxWordQueryLen {
			http.Error(w, fmt.Sprintf("query exceeds %d characters", maxWordQueryLen), http.StatusBadRequest)
			return
		}
		suggestions, err := pipe.SuggestWords(r.Context(), query)
		if err != nil {
			serverError(w, "suggest words", err)
			return
		}
		writeJSON(w, map[string]any{"suggestions": suggestions})
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
