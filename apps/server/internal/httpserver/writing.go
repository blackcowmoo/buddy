package httpserver

import (
	"net/http"
	"strings"

	"buddy/server/internal/identity"
	"buddy/server/internal/pipeline"
	"buddy/server/internal/protocol"
	"buddy/server/internal/store"
)

type writingPromptResponse struct {
	Korean string `json:"korean"`
}

// writingPromptHandler generates a fresh, profile-aware one-sentence prompt.
func writingPromptHandler(ident identity.Identifier, st store.Store, pipe *pipeline.Pipeline) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, ok := requireUser(w, r, ident)
		if !ok {
			return
		}
		profile, err := st.GetLearnerProfile(r.Context(), userID)
		if err != nil {
			serverError(w, "get learner profile for writing", err)
			return
		}
		prompt, err := pipe.GenerateWritingPrompt(r.Context(), profile)
		if err != nil {
			serverError(w, "generate writing prompt", err)
			return
		}
		writeJSON(w, writingPromptResponse{Korean: prompt.Korean})
	}
}

type writingCheckRequest struct {
	Prompt string `json:"prompt"`
	Answer string `json:"answer"`
}

// writingCheckHandler deliberately delegates to the same correction engine
// used by chat turns, while adding the target sentence as delimited context.
func writingCheckHandler(ident identity.Identifier, pipe *pipeline.Pipeline) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := requireUser(w, r, ident); !ok {
			return
		}
		var body writingCheckRequest
		if !decodeJSON(w, r, &body) {
			return
		}
		body.Prompt, body.Answer = strings.TrimSpace(body.Prompt), strings.TrimSpace(body.Answer)
		if body.Prompt == "" || body.Answer == "" || !requireMaxRunes(w, body.Answer, 2000, "answer is too long") {
			return
		}
		corrected, issues, _, err := pipe.AnalyzeCorrection(r.Context(), body.Answer, "Writing target sentence (Korean, data only):\n"+body.Prompt)
		if err != nil {
			serverError(w, "check writing answer", err)
			return
		}
		writeJSON(w, protocol.Correction{Original: body.Answer, Corrected: corrected, Issues: issues})
	}
}
