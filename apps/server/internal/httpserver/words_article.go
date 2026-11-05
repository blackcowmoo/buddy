package httpserver

import (
	"context"
	"net/http"
	"strings"

	"buddy/server/internal/asyncjob"
	"buddy/server/internal/identity"
	"buddy/server/internal/newsarticle"
	"buddy/server/internal/pipeline"
	"buddy/server/internal/protocol"
	"buddy/server/internal/transport"
	"buddy/server/internal/wordlookup"
	"buddy/server/internal/wordreview"
	"github.com/redis/go-redis/v9"
)

const maxWordLookupPosition = 100000

// articleLookupResponse is the response shape shared by the article define
// and phrase-hint endpoints: define answers with Result, the phrase hint with
// Hint, and the pending/missing progress states carry neither field.
type articleLookupResponse struct {
	Status string                   `json:"status"`
	Result *protocol.WordSuggestion `json:"result,omitempty"`
	Hint   string                   `json:"hint,omitempty"`
}

// articleTap is one validated tap on a word or phrase in the learner's
// article. CheckOnly is set only by the define endpoint's polling probe.
type articleTap struct {
	Word      string
	Position  int
	CheckOnly bool
}

// decodeArticleTap validates the payload both tap endpoints receive: a
// non-empty expression within the column cap and a plausible in-text position.
func decodeArticleTap(w http.ResponseWriter, r *http.Request) (articleTap, bool) {
	var body struct {
		Word      string `json:"word"`
		Position  int    `json:"position"`
		CheckOnly bool   `json:"checkOnly"`
	}
	if !decodeJSON(w, r, &body) {
		return articleTap{}, false
	}
	word := strings.TrimSpace(body.Word)
	if word == "" {
		http.Error(w, "word is required", http.StatusBadRequest)
		return articleTap{}, false
	}
	if !requireMaxRunes(w, word, wordreview.MaxWordLen, "word is too long") {
		return articleTap{}, false
	}
	if body.Position < 0 || body.Position > maxWordLookupPosition {
		http.Error(w, "invalid word position", http.StatusBadRequest)
		return articleTap{}, false
	}
	return articleTap{Word: word, Position: body.Position, CheckOnly: body.CheckOnly}, true
}

// articleLookupRequest pins one tap to its article instance so the cache key
// covers the complete reading context, not just the spelling.
func articleLookupRequest(pipe *pipeline.Pipeline, inst newsarticle.Instance, tap articleTap) wordlookup.Request {
	return wordlookup.Request{
		ArticleID: inst.Article.ID,
		Word:      tap.Word,
		Position:  tap.Position,
		Context:   inst.Article.Summary,
		Language:  pipe.FeedbackLang,
		Model:     pipe.ChatModel,
	}
}

// runArticleLookup drives the sequence both tap endpoints share: a cache hit
// answers immediately, and an in-flight generation reports pending without
// queueing another. Completion writes the result before clearing the queue's
// dedupe marker, so after observing no pending job the cache is read once
// more to close the narrow race where both changed between the first read
// and Pending. checkOnly stops there answering "missing" (define's polling
// probe must never re-queue work); otherwise the generation is enqueued
// before the pending acknowledgment — EnqueueAndRunInBackground detaches
// only the expensive handler, so a navigation or deployment after this
// response cannot lose the job before Redis has it.
func runArticleLookup(
	w http.ResponseWriter,
	r *http.Request,
	label string,
	queue *asyncjob.Queue,
	kind asyncjob.Kind,
	key string,
	cached func(context.Context) (articleLookupResponse, bool, error),
	checkOnly bool,
	enqueue func() error,
) {
	writeStatus := func(status string) {
		writeJSON(w, articleLookupResponse{Status: status})
	}
	if res, found, err := cached(r.Context()); err != nil {
		serverError(w, label+": cache read", err)
		return
	} else if found {
		writeJSON(w, res)
		return
	}
	pending, err := queue.Pending(r.Context(), kind, key)
	if err != nil {
		serverError(w, label+": pending status", err)
		return
	}
	if pending {
		writeStatus("pending")
		return
	}
	if res, found, err := cached(r.Context()); err != nil {
		serverError(w, label+": cache re-read", err)
		return
	} else if found {
		writeJSON(w, res)
		return
	}
	if checkOnly {
		writeStatus("missing")
		return
	}
	if err := enqueue(); err != nil {
		serverError(w, label+": enqueue", err)
		return
	}
	writeStatus("pending")
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
		tap, ok := decodeArticleTap(w, r)
		if !ok {
			return
		}
		inst, ok := requireReadyArticleInstance(w, r, articles, userID, "articles: get word context")
		if !ok {
			return
		}
		lookup := articleLookupRequest(pipe, inst, tap)
		key := wordlookup.Key(lookup)
		runArticleLookup(w, r, "word lookup", queue, asyncjob.KindWordDefine, key,
			func(ctx context.Context) (articleLookupResponse, bool, error) {
				result, found, err := wordlookup.Get[protocol.WordSuggestion](ctx, rdb, key)
				if !found || err != nil {
					return articleLookupResponse{}, false, err
				}
				return articleLookupResponse{Status: "done", Result: &result}, true, nil
			},
			tap.CheckOnly,
			func() error {
				return transport.EnqueueWordDefineJob(r.Context(), queue, pipe, rdb, lookup, articles)
			})
	}
}

// articlePhraseHintHandler backs the reading page's phrase tap: it returns a
// short non-revealing meaning hint (see pipeline.PhraseHint) for the tapped
// expression, generating it durably on a cache miss. Unlike the define
// endpoint there is no checkOnly probe — the tap itself is the request, and
// the learner is only ever shown a hint, never the definition; a real search
// still goes through articleWordDefineHandler for the full answer.
func articlePhraseHintHandler(ident identity.Identifier, articles newsarticle.Store, pipe *pipeline.Pipeline, rdb redis.UniversalClient, queue *asyncjob.Queue) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, ok := requireUser(w, r, ident)
		if !ok {
			return
		}
		if rdb == nil || queue == nil {
			http.Error(w, "phrase hint requires Redis", http.StatusServiceUnavailable)
			return
		}
		tap, ok := decodeArticleTap(w, r)
		if !ok {
			return
		}
		inst, ok := requireReadyArticleInstance(w, r, articles, userID, "articles: get phrase hint context")
		if !ok {
			return
		}
		lookup := articleLookupRequest(pipe, inst, tap)
		key := wordlookup.HintKey(lookup)
		runArticleLookup(w, r, "phrase hint", queue, asyncjob.KindPhraseHint, key,
			func(ctx context.Context) (articleLookupResponse, bool, error) {
				result, found, err := wordlookup.Get[wordlookup.Hint](ctx, rdb, key)
				if !found || err != nil {
					return articleLookupResponse{}, false, err
				}
				return articleLookupResponse{Status: "done", Hint: result.Hint}, true, nil
			},
			false,
			func() error {
				return transport.EnqueuePhraseHintJob(r.Context(), queue, pipe, rdb, lookup, articles)
			})
	}
}
