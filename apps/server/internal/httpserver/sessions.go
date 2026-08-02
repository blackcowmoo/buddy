package httpserver

import (
	"log"
	"net/http"
	"sync"

	"buddy/server/internal/identity"
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
