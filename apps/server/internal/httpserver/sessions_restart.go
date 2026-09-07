package httpserver

import (
	"context"
	"errors"
	"net/http"

	"buddy/server/internal/identity"
	"buddy/server/internal/store"
)

// sessionRestartSpec isolates the only differences between the study-summary
// and quiz regeneration endpoints. Both endpoints otherwise share the same
// authenticated load, eligibility, restart, dispatch, and response flow.
type sessionRestartSpec struct {
	eligible       func(store.SessionMeta) bool
	conflict       string
	restartContext string
	restart        func(context.Context, string, string) error
	dispatch       func(context.Context, string, string)
}

func sessionRestartHandler(ident identity.Identifier, st store.Store, spec sessionRestartSpec) http.HandlerFunc {
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
		if !meta.Ended || !spec.eligible(meta) {
			http.Error(w, spec.conflict, http.StatusConflict)
			return
		}
		if err := spec.restart(r.Context(), userID, sessionID); err != nil {
			serverError(w, spec.restartContext, err)
			return
		}

		spec.dispatch(r.Context(), userID, sessionID)
		w.WriteHeader(http.StatusNoContent)
	}
}
