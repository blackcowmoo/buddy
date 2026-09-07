package httpserver

import (
	"context"
	"net/http"

	"buddy/server/internal/asyncjob"
	"buddy/server/internal/identity"
	"buddy/server/internal/pipeline"
	"buddy/server/internal/store"
	"buddy/server/internal/transport"
	"buddy/server/internal/wordreview"
)

type wordAutoAddStatus struct {
	Status string `json:"status"`
	Count  int    `json:"count"`
}

// wordAutoAddHandler starts durable vocabulary generation, or reports an
// existing pending run instead of starting duplicate model work.
func wordAutoAddHandler(ident identity.Identifier, words wordreview.Store, st store.Store, pipe *pipeline.Pipeline, wordVerifyQueue, wordAutoAddQueue *asyncjob.Queue) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, ok := requireUser(w, r, ident)
		if !ok {
			return
		}
		status, count, err := st.GetWordAutoAddStatus(r.Context(), userID)
		if err != nil {
			serverError(w, "words: get auto-add status "+userID, err)
			return
		}
		if status == store.JobStatusPending {
			writeJSON(w, wordAutoAddStatus{Status: status, Count: count})
			return
		}
		if err := st.StartWordAutoAdd(r.Context(), userID); err != nil {
			serverError(w, "words: start auto-add "+userID, err)
			return
		}
		asyncjob.EnqueueOrRunInline(wordAutoAddQueue, r.Context(),
			"words: enqueue auto-add "+userID,
			func(ctx context.Context) error {
				return transport.EnqueueWordAutoAddJob(ctx, wordAutoAddQueue, pipe, words, st, wordVerifyQueue, userID)
			},
			"words: auto-add "+userID,
			func(ctx context.Context) error {
				return transport.RunWordAutoAddInline(ctx, pipe, words, st, wordVerifyQueue, userID)
			},
		)
		writeJSON(w, wordAutoAddStatus{Status: store.JobStatusPending})
	}
}

func wordAutoAddStatusHandler(ident identity.Identifier, st store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, ok := requireUser(w, r, ident)
		if !ok {
			return
		}
		status, count, err := st.GetWordAutoAddStatus(r.Context(), userID)
		if err != nil {
			serverError(w, "words: get auto-add status "+userID, err)
			return
		}
		writeJSON(w, wordAutoAddStatus{Status: status, Count: count})
	}
}
