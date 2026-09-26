package httpserver

import (
	"context"
	"log"
	"net/http"
	"sync"

	"buddy/server/internal/asyncjob"
	"buddy/server/internal/identity"
	"buddy/server/internal/pipeline"
	"buddy/server/internal/transport"
	"buddy/server/internal/wordreview"
)

func wordMeaningCleanupScheduler(words wordreview.Store, pipe *pipeline.Pipeline, queue *asyncjob.Queue) func(context.Context, string) {
	var running sync.Map
	return func(ctx context.Context, userID string) {
		if queue != nil {
			if err := transport.EnqueueWordMeaningCleanup(ctx, queue, pipe, words, userID); err != nil {
				// Pending intent remains in MySQL; a later list refresh retries
				// enqueueing without silently switching away from durable execution.
				log.Printf("words: enqueue meaning cleanup: %v", err)
			}
			return
		}
		if _, loaded := running.LoadOrStore(userID, struct{}{}); loaded {
			return
		}
		go func() {
			defer running.Delete(userID)
			if err := transport.RunWordMeaningCleanupInline(context.Background(), pipe, words, userID); err != nil {
				log.Printf("words: meaning cleanup: %v", err)
			}
		}()
	}
}

func wordMeaningCleanupHandler(ident identity.Identifier, words wordreview.Store, schedule func(context.Context, string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, ok := requireUser(w, r, ident)
		if !ok {
			return
		}
		store, ok := words.(wordreview.MeaningStore)
		if !ok {
			http.Error(w, "meaning cleanup unavailable", http.StatusServiceUnavailable)
			return
		}
		if err := store.StartMeaningCleanup(r.Context(), userID); err != nil {
			serverError(w, "words: start meaning cleanup", err)
			return
		}
		schedule(r.Context(), userID)
		writeJSON(w, map[string]bool{"started": true})
	}
}
