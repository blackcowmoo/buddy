package httpserver

import (
	"context"
	"net/http"
	"strings"

	"buddy/server/internal/asyncjob"
	"buddy/server/internal/identity"
	"buddy/server/internal/pipeline"
	"buddy/server/internal/protocol"
	"buddy/server/internal/transport"
	"buddy/server/internal/writing"
)

type writingPromptResponse struct {
	ID        string `json:"id"`
	Korean    string `json:"korean"`
	Status    string `json:"status"`
	CreatedAt int64  `json:"createdAt"`
}

func toWritingResponse(p writing.Prompt) writingPromptResponse {
	return writingPromptResponse{ID: p.ID, Korean: p.Korean, Status: p.Status, CreatedAt: p.CreatedAt.Unix()}
}

func writingListHandler(ident identity.Identifier, st writing.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, ok := requireUser(w, r, ident)
		if !ok {
			return
		}
		items, err := st.List(r.Context(), userID)
		if err != nil {
			serverError(w, "list writing prompts", err)
			return
		}
		out := make([]writingPromptResponse, len(items))
		for i, p := range items {
			out[i] = toWritingResponse(p)
		}
		writeJSON(w, out)
	}
}

func writingInstanceHandler(ident identity.Identifier, st writing.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, ok := requireUser(w, r, ident)
		if !ok {
			return
		}
		p, err := st.Get(r.Context(), userID, r.PathValue("id"))
		if err != nil {
			serverError(w, "get writing prompt", err)
			return
		}
		if p.ID == "" {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, toWritingResponse(p))
	}
}

func writingDrawHandler(ident identity.Identifier, st writing.Store, profile storeProfile, pipe *pipeline.Pipeline, q *asyncjob.Queue) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, ok := requireUser(w, r, ident)
		if !ok {
			return
		}
		p, err := st.Create(r.Context(), userID)
		if err != nil {
			serverError(w, "create writing prompt", err)
			return
		}
		asyncjob.EnqueueOrRunInline(q, r.Context(), "writing: enqueue prompt", func(ctx context.Context) error {
			return transport.EnqueueWritingPromptJob(ctx, q, pipe, st, profile, p.ID, userID)
		}, "writing: generate prompt", func(ctx context.Context) error {
			return transport.RunWritingPromptInline(ctx, pipe, st, profile, p.ID, userID)
		})
		writeJSON(w, toWritingResponse(p))
	}
}

type storeProfile func(context.Context, string) (string, error)

type writingCheckRequest struct {
	Prompt string `json:"prompt"`
	Answer string `json:"answer"`
}

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
