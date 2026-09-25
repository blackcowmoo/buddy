package httpserver

import (
	"net/http"
	"strings"

	"buddy/server/internal/asyncjob"
	"buddy/server/internal/identity"
	"buddy/server/internal/newsarticle"
	"buddy/server/internal/pipeline"
	"buddy/server/internal/protocol"
	"buddy/server/internal/transport"
	"buddy/server/internal/wordlookup"
	"github.com/redis/go-redis/v9"
)

const maxWordLookupPosition = 100000

type articleWordLookupResponse struct {
	Status string                   `json:"status"`
	Result *protocol.WordSuggestion `json:"result,omitempty"`
}

// articleWordDefineHandler derives context from the caller's stored article
// instance. Cache misses are durably queued while hits return immediately.
func articleWordDefineHandler(ident identity.Identifier, articles newsarticle.Store, pipe *pipeline.Pipeline, rdb redis.UniversalClient, queue *asyncjob.Queue) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, ok := requireUser(w, r, ident)
		if !ok {
			return
		}
		if rdb == nil || queue == nil {
			http.Error(w, "word lookup requires Redis", http.StatusServiceUnavailable)
			return
		}
		var body struct {
			Word      string `json:"word"`
			Position  int    `json:"position"`
			CheckOnly bool   `json:"checkOnly"`
		}
		if !decodeJSON(w, r, &body) {
			return
		}
		word := strings.TrimSpace(body.Word)
		if word == "" {
			http.Error(w, "word is required", http.StatusBadRequest)
			return
		}
		if !requireMaxRunes(w, word, maxWordLen, "word is too long") {
			return
		}
		if body.Position < 0 || body.Position > maxWordLookupPosition {
			http.Error(w, "invalid word position", http.StatusBadRequest)
			return
		}
		inst, err := articles.Get(r.Context(), userID, r.PathValue("id"))
		if err != nil {
			serverError(w, "articles: get word context "+userID, err)
			return
		}
		if inst.ID == "" {
			http.NotFound(w, r)
			return
		}
		if inst.Article.Status != newsarticle.StatusDone {
			http.Error(w, "article study still generating", http.StatusConflict)
			return
		}
		lookup := wordlookup.Request{
			ArticleID: inst.Article.ID,
			Word:      word,
			Position:  body.Position,
			Context:   inst.Article.Summary,
			Language:  pipe.FeedbackLang,
			Model:     pipe.ChatModel,
		}
		key := wordlookup.Key(lookup)
		writeCachedResult := func() (bool, error) {
			result, found, err := wordlookup.Get(r.Context(), rdb, key)
			if err != nil || !found {
				return found, err
			}
			writeJSON(w, articleWordLookupResponse{Status: "done", Result: &result})
			return true, nil
		}
		if found, err := writeCachedResult(); err != nil {
			serverError(w, "word lookup: cache read", err)
			return
		} else if found {
			return
		}
		pending, err := queue.Pending(r.Context(), asyncjob.KindWordDefine, key)
		if err != nil {
			serverError(w, "word lookup: pending status", err)
			return
		}
		if pending {
			writeJSON(w, articleWordLookupResponse{Status: "pending"})
			return
		}
		// Completion writes the result before clearing the queue's dedupe
		// marker. Re-read after observing no pending job to close the narrow
		// race where both changed between the first cache read and Pending.
		if found, err := writeCachedResult(); err != nil {
			serverError(w, "word lookup: cache re-read", err)
			return
		} else if found {
			return
		}
		if body.CheckOnly {
			writeJSON(w, articleWordLookupResponse{Status: "missing"})
			return
		}
		// Enqueue before acknowledging pending. EnqueueAndRunInBackground
		// detaches only the expensive handler, so a navigation or deployment
		// after this response cannot lose the job before Redis has it.
		if err := transport.EnqueueWordDefineJob(r.Context(), queue, pipe, rdb, lookup, articles); err != nil {
			serverError(w, "word lookup: enqueue", err)
			return
		}
		writeJSON(w, articleWordLookupResponse{Status: "pending"})
	}
}
