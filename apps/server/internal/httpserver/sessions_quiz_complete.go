package httpserver

import (
	"net/http"

	"buddy/server/internal/identity"
	"buddy/server/internal/store"
)

// sessionQuizCompleteHandler marks an ended session's quiz as studied (see
// SessionMeta.QuizCompleted's doc comment) — called by the frontend either
// once a learner finishes every quiz question, right or wrong (grading
// happens entirely client-side in QuizPanel, the same trust-the-client model
// normalizeQuizAnswer already used before this endpoint existed — this is a
// "studied it" checkmark, not a "got it right" one), or, for a session whose
// quiz came back with no questions at all, when the learner taps "내가
// 읽었음" (I've read it) instead — both cases mean the same thing for the
// room list's badge: this session's feedback has been studied.
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
