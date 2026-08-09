package httpserver

import (
	"context"
	"fmt"
	"log"
	"net/http"

	"buddy/server/internal/asyncjob"
	"buddy/server/internal/concurrent"
	"buddy/server/internal/identity"
	"buddy/server/internal/pipeline"
	"buddy/server/internal/recording"
	"buddy/server/internal/store"
	"buddy/server/internal/transport"
)

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

// instantSessionsListHandler returns the caller's own instant/"오늘의 한 문장"
// rooms (see store.MySQLStore.ListInstantSessions) — the mirror image of
// sessionsListHandler's own exclusion of them, for that feature's own
// dedicated list page (apps/web/src/pages/InstantSessions.tsx). Same
// identity scoping as sessionsListHandler.
func instantSessionsListHandler(ident identity.Identifier, st store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, ok := requireUser(w, r, ident)
		if !ok {
			return
		}
		sessions, err := st.ListInstantSessions(r.Context(), userID)
		if err != nil {
			serverError(w, "list instant sessions", err)
			return
		}
		writeJSON(w, sessions)
	}
}

// sessionMarkInstantHandler flags a brand-new room as an instant/"오늘의 한
// 문장" conversation, called by the client right after it learns the
// server-minted session ID (the "ready" WS event) — see
// store.MySQLStore.MarkInstant for why this has to be its own
// race-tolerant upsert rather than piggybacking on SaveTurn's.
func sessionMarkInstantHandler(ident identity.Identifier, st store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, ok := requireUser(w, r, ident)
		if !ok {
			return
		}
		sessionID := r.PathValue("id")
		if err := st.MarkInstant(r.Context(), userID, sessionID); err != nil {
			serverError(w, "mark instant", err)
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
//
// If the deleted session had actually folded a study summary into the
// learner's cross-session profile (ended, non-empty study summary), this
// also kicks off a from-scratch profile rebuild (see
// transport.EnqueueProfileRegenerateJob/RunProfileRegenerateInline and
// asyncjob.KindProfileRegenerate's doc comment) — otherwise a deleted
// session's influence would linger in the profile forever, even though the
// whole point of deleting it (e.g. a wrong/bad reply) was to keep it out of
// future study material. Checked and gated on before the delete, but
// enqueued after: the rebuild reads whatever sessions are left via
// store.Store.ListSessionsWithStudySummary, which must not still include
// the one being deleted.
func sessionDeleteHandler(ident identity.Identifier, st store.Store, audio transport.AudioSaver, recordings recording.Store, pipe *pipeline.Pipeline, profileRegenerateQueue *asyncjob.Queue) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, ok := requireUser(w, r, ident)
		if !ok {
			return
		}
		sessionID := r.PathValue("id")

		// Any error here (including "doesn't exist") just means "nothing to
		// rebuild for" — never blocks the delete itself, which is why this
		// isn't wired into the fns/error-handling below.
		contributedToProfile := false
		if meta, _, err := st.SessionDetail(r.Context(), userID, sessionID); err == nil {
			contributedToProfile = meta.Ended && len(meta.StudySummary) > 0
		}

		var fns []func()
		if audio != nil {
			fns = append(fns, func() {
				if err := audio.DeleteBySession(r.Context(), userID, sessionID); err != nil {
					log.Printf("delete session: cascade audio backups %s/%s: %v", userID, sessionID, err)
				}
			})
		}
		if recordings != nil {
			fns = append(fns, func() {
				if err := recordings.DeleteBySession(r.Context(), userID, sessionID); err != nil {
					log.Printf("delete session: cascade recordings %s/%s: %v", userID, sessionID, err)
				}
			})
		}
		concurrent.Run(fns...)
		if err := st.DeleteSession(r.Context(), userID, sessionID); err != nil {
			serverError(w, "delete session", err)
			return
		}

		if contributedToProfile {
			asyncjob.EnqueueOrRunInline(profileRegenerateQueue, r.Context(),
				fmt.Sprintf("delete session: enqueue profile regenerate %s", userID),
				func(ctx context.Context) error {
					return transport.EnqueueProfileRegenerateJob(ctx, profileRegenerateQueue, pipe, st, userID)
				},
				fmt.Sprintf("delete session: regenerate learner profile %s", userID),
				func(ctx context.Context) error {
					return transport.RunProfileRegenerateInline(ctx, pipe, st, userID)
				},
			)
		}

		w.WriteHeader(http.StatusNoContent)
	}
}
