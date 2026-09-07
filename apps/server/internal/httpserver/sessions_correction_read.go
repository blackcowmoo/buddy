package httpserver

import (
	"net/http"
	"strconv"

	"buddy/server/internal/identity"
	"buddy/server/internal/store"
)

// sessionCorrectionReadHandler acknowledges one final Judge correction only
// after the learner actually opens it. The write is deliberately scoped to
// (user, session, turn); a guessed room ID cannot clear another learner's
// notification, and acknowledging a still-refining Chat preview is a no-op in
// the store until a terminal result exists.
func sessionCorrectionReadHandler(ident identity.Identifier, st store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, ok := requireUser(w, r, ident)
		if !ok {
			return
		}
		turn, err := strconv.Atoi(r.PathValue("turn"))
		if err != nil || turn < 1 {
			http.Error(w, "invalid turn", http.StatusBadRequest)
			return
		}
		if err := st.MarkCorrectionRead(r.Context(), userID, r.PathValue("id"), turn); err != nil {
			serverError(w, "mark correction read", err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}
