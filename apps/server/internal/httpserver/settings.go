package httpserver

import (
	"fmt"
	"net/http"
	"strings"

	"buddy/server/internal/identity"
	"buddy/server/internal/store"
)

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
		if !decodeJSON(w, r, &body) {
			return
		}
		style := strings.TrimSpace(body.InterlocutorStyle)
		if !requireMaxRunes(w, style, maxInterlocutorStyleLen, fmt.Sprintf("interlocutorStyle exceeds %d characters", maxInterlocutorStyleLen)) {
			return
		}
		if err := st.SaveInterlocutorStyle(r.Context(), userID, style); err != nil {
			serverError(w, "save interlocutor style", err)
			return
		}
		writeJSON(w, map[string]any{"interlocutorStyle": style})
	}
}
