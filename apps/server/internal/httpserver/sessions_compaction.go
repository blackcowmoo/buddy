package httpserver

import (
	"net/http"

	"buddy/server/internal/concurrent"
	"buddy/server/internal/identity"
	"buddy/server/internal/store"
)

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
		concurrent.Run(
			func() { profile, loadErr = st.Load(r.Context(), userID, sessionID) },
			func() { lastTurn, lastTurnErr = st.LastTurn(r.Context(), userID, sessionID) },
		)
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
