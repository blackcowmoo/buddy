package httpserver

import (
	"fmt"
	"net/http"
	"strings"

	"buddy/server/internal/identity"
	"buddy/server/internal/pipeline"
)

const (
	maxWordQueryLen         = 200
	maxWordDefineContextLen = 2000
)

// wordSuggestHandler resolves a native-language description into candidate
// English words. Nothing is persisted, so this latency-sensitive lookup stays
// synchronous.
func wordSuggestHandler(ident identity.Identifier, pipe *pipeline.Pipeline) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := requireUser(w, r, ident); !ok {
			return
		}
		var body struct {
			Query string `json:"query"`
		}
		if !decodeJSON(w, r, &body) {
			return
		}
		query := strings.TrimSpace(body.Query)
		if query == "" {
			http.Error(w, "query is required", http.StatusBadRequest)
			return
		}
		if !requireMaxRunes(w, query, maxWordQueryLen, fmt.Sprintf("query exceeds %d characters", maxWordQueryLen)) {
			return
		}
		suggestions, err := pipe.SuggestWords(r.Context(), query)
		if err != nil {
			serverError(w, "suggest words", err)
			return
		}
		writeJSON(w, map[string]any{"suggestions": suggestions})
	}
}

// wordDefineHandler defines a selected English word in caller-supplied
// context. Saving remains a separate explicit /api/words/save action.
func wordDefineHandler(ident identity.Identifier, pipe *pipeline.Pipeline) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := requireUser(w, r, ident); !ok {
			return
		}
		var body struct {
			Word    string `json:"word"`
			Context string `json:"context"`
		}
		if !decodeJSON(w, r, &body) {
			return
		}
		word := strings.TrimSpace(body.Word)
		wordContext := strings.TrimSpace(body.Context)
		if word == "" {
			http.Error(w, "word is required", http.StatusBadRequest)
			return
		}
		if !requireMaxRunes(w, word, maxWordLen, "word is too long") {
			return
		}
		if !requireMaxRunes(w, wordContext, maxWordDefineContextLen, fmt.Sprintf("context exceeds %d characters", maxWordDefineContextLen)) {
			return
		}
		suggestion, err := pipe.DefineWord(r.Context(), word, wordContext)
		if err != nil {
			serverError(w, "define word", err)
			return
		}
		writeJSON(w, suggestion)
	}
}
