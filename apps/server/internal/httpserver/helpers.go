package httpserver

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"unicode/utf8"

	"buddy/server/internal/asyncjob"
	"buddy/server/internal/identity"
	"buddy/server/internal/recording"
)

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// requireUser resolves the caller's identity the way every JSON-API handler
// needs to, writing a 401 and reporting false on failure so callers can
// `userID, ok := requireUser(...); if !ok { return }`.
func requireUser(w http.ResponseWriter, r *http.Request, ident identity.Identifier) (string, bool) {
	userID, ok := ident.Identify(w, r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return "", false
	}
	return userID, true
}

// requireRecordings guards the recording-endpoint handlers against a nil
// Store (archival disabled), writing a 503 and reporting false on failure so
// callers can `if !requireRecordings(w, recordings) { return }`.
func requireRecordings(w http.ResponseWriter, recordings recording.Store) bool {
	if recordings == nil {
		http.Error(w, "recording storage is not configured", http.StatusServiceUnavailable)
		return false
	}
	return true
}

// serverError logs err with context and writes a generic 500 — the response
// body never leaks internal error detail to the caller.
func serverError(w http.ResponseWriter, context string, err error) {
	log.Printf("%s: %v", context, err)
	http.Error(w, "internal error", http.StatusInternalServerError)
}

// decodeJSON decodes r's JSON body into v, writing a 400 and reporting false
// on failure so callers can `if !decodeJSON(w, r, &body) { return }`.
func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return false
	}
	return true
}

// requireMaxRunes writes msg as a 400 and reports false if s exceeds max
// runes, so callers can `if !requireMaxRunes(w, s, max, msg) { return }`.
func requireMaxRunes(w http.ResponseWriter, s string, max int, msg string) bool {
	if utf8.RuneCountInString(s) > max {
		http.Error(w, msg, http.StatusBadRequest)
		return false
	}
	return true
}

// enqueueOrRunInline is the shared "durable queue if Redis is configured,
// otherwise a detached best-effort goroutine" fallback behind
// sessionEndHandler's and sessionRestudyHandler's study-summary/quiz kickoff:
// when queue is non-nil, enqueue runs synchronously on ctx (cheap — no LLM
// call — the queue's own EnqueueAndRunInBackground handles backgrounding the
// actual work); otherwise inline runs the whole job itself, so it must be
// backgrounded here on a detached context.Background() goroutine to get the
// same "outlives this response" behavior without Redis.
func enqueueOrRunInline(queue *asyncjob.Queue, ctx context.Context, enqueueErrLabel string, enqueue func(ctx context.Context) error, inlineErrLabel string, inline func(ctx context.Context) error) {
	if queue != nil {
		if err := enqueue(ctx); err != nil {
			log.Printf("%s: %v", enqueueErrLabel, err)
		}
		return
	}
	go func() {
		if err := inline(context.Background()); err != nil {
			log.Printf("%s: %v", inlineErrLabel, err)
		}
	}()
}
