package httpserver

import (
	"context"
	"encoding/json"
	"net/http"

	"buddy/server/internal/asyncjob"
	"buddy/server/internal/identity"
	"buddy/server/internal/pipeline"
	"buddy/server/internal/transport"
	"buddy/server/internal/wordreview"
)

func wordResearchHandler(ident identity.Identifier, words wordreview.Store, pipe *pipeline.Pipeline, queue *asyncjob.Queue) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, ok := requireUser(w, r, ident)
		if !ok {
			return
		}
		target, err := words.Get(r.Context(), userID, r.PathValue("id"))
		if err != nil {
			serverError(w, "words: research get", err)
			return
		}
		if target.ID == "" {
			http.NotFound(w, r)
			return
		}
		store, ok := words.(wordreview.ResearchStore)
		if !ok {
			http.Error(w, "research is unavailable", http.StatusServiceUnavailable)
			return
		}
		updated, err := store.StartResearch(r.Context(), userID, target.ID)
		if err != nil {
			serverError(w, "words: research start", err)
			return
		}
		asyncjob.EnqueueOrRunInline(queue, r.Context(), "words: enqueue research", func(ctx context.Context) error {
			return transport.EnqueueWordResearchJob(ctx, queue, pipe, words, userID, target.ID)
		}, "words: research", func(ctx context.Context) error {
			return transport.WordResearchJobHandler(pipe, words)(ctx, asyncjob.Job{Kind: asyncjob.KindWordResearch, Payload: mustResearchPayload(userID, target.ID)})
		})
		writeJSON(w, toWordItem(updated))
	}
}

func mustResearchPayload(userID, wordID string) json.RawMessage {
	payload, _ := json.Marshal(map[string]string{"UserID": userID, "WordID": wordID})
	return payload
}

func wordResearchConfirmHandler(ident identity.Identifier, words wordreview.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, ok := requireUser(w, r, ident)
		if !ok {
			return
		}
		store, ok := words.(wordreview.ResearchStore)
		if !ok {
			http.Error(w, "research is unavailable", http.StatusServiceUnavailable)
			return
		}
		target, err := words.Get(r.Context(), userID, r.PathValue("id"))
		if err != nil {
			serverError(w, "words: research duplicate lookup", err)
			return
		}
		if target.ID == "" {
			http.NotFound(w, r)
			return
		}
		list, err := words.List(r.Context(), userID)
		if err != nil {
			serverError(w, "words: research duplicate list", err)
			return
		}
		for _, existing := range list {
			if existing.ID == target.ID || wordreview.NormalizeWord(existing.Word) != wordreview.NormalizeWord(target.Word) || existing.Meaning != target.Meaning {
				continue
			}
			if err := words.Delete(r.Context(), userID, target.ID); err != nil {
				serverError(w, "words: research duplicate delete", err)
				return
			}
			writeJSON(w, toWordItem(existing))
			return
		}
		updated, err := store.ConfirmResearch(r.Context(), userID, r.PathValue("id"))
		if err != nil {
			serverError(w, "words: research confirm", err)
			return
		}
		if updated.ID == "" {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, toWordItem(updated))
	}
}
