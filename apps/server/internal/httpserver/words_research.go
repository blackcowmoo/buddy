package httpserver

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"buddy/server/internal/asyncjob"
	"buddy/server/internal/identity"
	"buddy/server/internal/pipeline"
	"buddy/server/internal/transport"
	"buddy/server/internal/wordreview"
)

func wordResearchHandler(ident identity.Identifier, words wordreview.Store, pipe *pipeline.Pipeline, queue *asyncjob.Queue) http.HandlerFunc {
	var inline asyncjob.InlineRunner
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
		if target.ResearchStatus == wordreview.ResearchPending {
			writeJSON(w, toWordItem(target))
			return
		}
		if target.Status == wordreview.StatusPending || wordreview.MeaningNeedsReview(target) {
			http.Error(w, "word verification or meaning review is still pending", http.StatusConflict)
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
		if updated.ID == "" {
			http.NotFound(w, r)
			return
		}
		asyncjob.EnqueueOrRunInline(queue, r.Context(), "words: enqueue research", func(ctx context.Context) error {
			return transport.EnqueueWordResearchJob(ctx, queue, pipe, words, userID, target.ID, updated.ResearchRevision)
		}, "words: research", func(context.Context) error {
			inline.Start(fmt.Sprintf("%s/%s:r%d", userID, target.ID, updated.ResearchRevision), "words: research", func(ctx context.Context) error {
				return transport.RunWordResearchInline(ctx, pipe, words, userID, target.ID, updated.ResearchRevision)
			})
			return nil
		})
		writeJSON(w, toWordItem(updated))
	}
}

func wordResearchSelectionHandler(ident identity.Identifier, words wordreview.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, ok := requireUser(w, r, ident)
		if !ok {
			return
		}
		store, ok := words.(wordreview.ResearchSelectionStore)
		if !ok {
			http.Error(w, "research selection is unavailable", http.StatusServiceUnavailable)
			return
		}
		var body struct {
			Revision   int                           `json:"revision"`
			Suggestion wordreview.ResearchSuggestion `json:"suggestion"`
		}
		if !decodeJSON(w, r, &body) {
			return
		}
		updated, err := store.SelectResearch(r.Context(), userID, r.PathValue("id"), body.Revision, body.Suggestion)
		if errors.Is(err, wordreview.ErrResearchConflict) {
			http.Error(w, "research selection is stale or conflicts with an existing word", http.StatusConflict)
			return
		}
		if err != nil {
			serverError(w, "words: research select", err)
			return
		}
		if updated.ID == "" {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, toWordItem(updated))
	}
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
		// ConfirmResearch deliberately lets a learner restore a rejected word,
		// but it must not short-circuit the initial model verification. This
		// protects against stale or non-browser clients even though the current
		// UI also disables confirmation while status is pending.
		if target.Status == wordreview.StatusPending || target.ResearchStatus == wordreview.ResearchPending {
			http.Error(w, "word verification is still pending", http.StatusConflict)
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
