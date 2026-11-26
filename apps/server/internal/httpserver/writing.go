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

	// Answer and check fields describe the last submitted answer and the
	// asynchronously produced result, so reopening the page restores them.
	Answer      string           `json:"answer,omitempty"`
	CheckStatus string           `json:"checkStatus,omitempty"`
	Corrected   string           `json:"corrected,omitempty"`
	Issues      []protocol.Issue `json:"issues,omitempty"`
}

func toWritingResponse(p writing.Prompt) writingPromptResponse {
	return writingPromptResponse{
		ID: p.ID, Korean: p.Korean, Status: p.Status, CreatedAt: p.CreatedAt.Unix(),
		Answer: p.Answer, CheckStatus: p.CheckStatus, Corrected: p.Corrected, Issues: p.Issues,
	}
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

// writingInstanceHandler returns one prompt with its persisted answer and
// check result. While a check is pending it re-dispatches the job: the
// dispatch is deduped by prompt ID, so this only replaces a dispatch that
// was lost (worker died between the submit and its run) and every poll of a
// running check collapses into the one job in flight.
func writingInstanceHandler(ident identity.Identifier, st writing.Store, pipe *pipeline.Pipeline, checks *asyncjob.Queue) http.HandlerFunc {
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
		if p.CheckStatus == writing.CheckPending {
			if err := transport.DispatchWritingCheck(context.Background(), checks, pipe, st, p.ID, userID); err != nil {
				serverError(w, "writing: dispatch check", err)
				return
			}
		}
		writeJSON(w, toWritingResponse(p))
	}
}

func writingDeleteHandler(ident identity.Identifier, st writing.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, ok := requireUser(w, r, ident)
		if !ok {
			return
		}
		if err := st.Delete(r.Context(), userID, r.PathValue("id")); err != nil {
			serverError(w, "delete writing prompt", err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
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
	PromptID string `json:"promptId"`
	Answer   string `json:"answer"`
}

// writingCheckHandler persists the submitted answer, marks its check
// pending, and hands grading to the detached writing-check job — the LLM is
// never consulted on the request's context, so navigating away mid-check
// neither cancels the grade nor loses the answer. The response is the
// prompt with checkStatus "pending"; the page polls the instance route (or
// just reopens it) to pick up the finished result.
func writingCheckHandler(ident identity.Identifier, st writing.Store, pipe *pipeline.Pipeline, checks *asyncjob.Queue) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, ok := requireUser(w, r, ident)
		if !ok {
			return
		}
		var body writingCheckRequest
		if !decodeJSON(w, r, &body) {
			return
		}
		body.PromptID = strings.TrimSpace(body.PromptID)
		body.Answer = strings.TrimSpace(body.Answer)
		if body.PromptID == "" {
			http.Error(w, "promptId is required", http.StatusBadRequest)
			return
		}
		if body.Answer == "" {
			http.Error(w, "answer is required", http.StatusBadRequest)
			return
		}
		if !requireMaxRunes(w, body.Answer, 2000, "answer is too long") {
			return
		}
		ctx := r.Context()
		p, err := st.Get(ctx, userID, body.PromptID)
		if err != nil {
			serverError(w, "get writing prompt", err)
			return
		}
		if p.ID == "" {
			http.NotFound(w, r)
			return
		}
		if p.Status != writing.StatusDone || p.Korean == "" {
			http.Error(w, "prompt is not ready", http.StatusConflict)
			return
		}
		started, err := st.StartCheck(ctx, userID, p.ID, body.Answer)
		if err != nil {
			serverError(w, "start writing check", err)
			return
		}
		if !started {
			// The prompt was deleted between the read and the write.
			http.NotFound(w, r)
			return
		}
		// A failed dispatch (e.g. a transient Redis outage) leaves the row
		// pending; the instance route's read-time re-dispatch converges it,
		// so the submission still counts even if this returns an error.
		if err := transport.DispatchWritingCheck(context.Background(), checks, pipe, st, p.ID, userID); err != nil {
			serverError(w, "writing: dispatch check", err)
			return
		}
		p.Answer, p.CheckStatus, p.Corrected, p.Issues = body.Answer, writing.CheckPending, "", nil
		writeJSON(w, toWritingResponse(p))
	}
}

// writingRecheckHandler answers the page's "다시 검사" request: it clears the
// stored result, marks the stored answer's check pending again, and
// re-dispatches — the learner never retypes. Repeated clicks while a check
// is running claim nothing and simply return the running check.
func writingRecheckHandler(ident identity.Identifier, st writing.Store, pipe *pipeline.Pipeline, checks *asyncjob.Queue) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, ok := requireUser(w, r, ident)
		if !ok {
			return
		}
		id := r.PathValue("id")
		started, err := st.Recheck(r.Context(), userID, id)
		if err != nil {
			serverError(w, "recheck writing answer", err)
			return
		}
		p, err := st.Get(r.Context(), userID, id)
		if err != nil {
			serverError(w, "get writing prompt", err)
			return
		}
		if p.ID == "" {
			http.NotFound(w, r)
			return
		}
		if !started {
			// Nothing to redo: no stored answer (or a check already in
			// flight). Either way the stored state is the answer.
			writeJSON(w, toWritingResponse(p))
			return
		}
		if err := transport.DispatchWritingCheck(context.Background(), checks, pipe, st, p.ID, userID); err != nil {
			serverError(w, "writing: dispatch check", err)
			return
		}
		writeJSON(w, toWritingResponse(p))
	}
}
