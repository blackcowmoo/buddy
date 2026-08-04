package httpserver

import (
	"errors"
	"net/http"

	"buddy/server/internal/asyncjob"
	"buddy/server/internal/identity"
	"buddy/server/internal/pipeline"
	"buddy/server/internal/store"
	"buddy/server/internal/transport"
)

// sessionEndHandler permanently marks one chat room read-only the instant
// the learner confirms "end this conversation" (see EndConversationControl
// in apps/web/src/App.tsx), and kicks off its study-summary/quiz generation
// from there — see transport.FinalizeSession, the exact same path
// CorrectionJobHandler's own instant-conversation auto-finalize goes
// through, so a learner-confirmed end and an instant room's automatic one
// behave identically.
func sessionEndHandler(ident identity.Identifier, st store.Store, pipe *pipeline.Pipeline, studySummaryQueue *asyncjob.Queue, studyQuizQueue *asyncjob.Queue) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, ok := requireUser(w, r, ident)
		if !ok {
			return
		}
		sessionID := r.PathValue("id")

		if err := transport.FinalizeSession(r.Context(), st, pipe, studySummaryQueue, studyQuizQueue, userID, sessionID); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				http.NotFound(w, r)
				return
			}
			serverError(w, "end session", err)
			return
		}

		w.WriteHeader(http.StatusNoContent)
	}
}
