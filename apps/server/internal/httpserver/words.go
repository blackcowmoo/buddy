package httpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"buddy/server/internal/asyncjob"
	"buddy/server/internal/identity"
	"buddy/server/internal/pipeline"
	"buddy/server/internal/transport"
	"buddy/server/internal/wordreview"
)

// maxWordLen/maxWordFieldLen cap what a learner can persist via
// wordSaveHandler — generous enough for a phrase or idiom (not just a single
// word, see wordreview's package doc) and its meaning/example, same
// abuse-guard reasoning as wordSuggestHandler's maxWordQueryLen.
const (
	maxWordLen      = 255 // matches buddy_word_reviews.word's VARCHAR(255)
	maxWordFieldLen = 2000
)

// wordItem mirrors one wordreview.Word for the frontend — a subset of the
// stored fields (no CorrectStreak, which isn't shown anywhere yet). No
// "retired" flag: words are never removed from review rotation, just
// reviewed less and less often as NextReviewAt drifts further out (see
// wordreview.stageIntervals' doc for why there's no ceiling).
type wordItem struct {
	ID           string `json:"id"`
	Word         string `json:"word"`
	Meaning      string `json:"meaning"`
	Example      string `json:"example"`
	Stage        int    `json:"stage"`
	ReviewCount  int    `json:"reviewCount"`
	NextReviewAt int64  `json:"nextReviewAt"` // unix seconds
	// Status is "pending" (still being fact-checked in the background),
	// "verified" (passed, in normal review rotation), or "rejected" (failed
	// the model-consensus check — see VerifyReason). See wordreview.Status*.
	Status       string `json:"status"`
	VerifyReason string `json:"verifyReason,omitempty"`
}

func toWordItem(w wordreview.Word) wordItem {
	return wordItem{
		ID:           w.ID,
		Word:         w.Word,
		Meaning:      w.Meaning,
		Example:      w.Example,
		Stage:        w.Stage,
		ReviewCount:  w.ReviewCount,
		NextReviewAt: w.NextReviewAt.Unix(),
		Status:       w.Status,
		VerifyReason: w.VerifyReason,
	}
}

// wordSaveHandler adds one word/phrase/idiom the learner explicitly chose to
// study (the "학습하기" button on a word-search suggestion, see
// WordSearchControl in apps/web/src/App.tsx) to their spaced-repetition
// study list. Deliberately narrow — it never saves a whole batch of search
// suggestions at once, only the single one the learner picked; see
// wordreview's package doc for why.
//
// The response comes back the instant the row is saved (status "pending")
// — it never waits on pipe.VerifyWord, which makes several LLM calls (see
// pipeline.minWordVerifyJudges) against a possibly slow local model. That
// check is kicked off separately right after, the same "durable queue when
// Redis is configured, detached inline goroutine otherwise"
// enqueueOrRunInline pattern sessionEndHandler uses for the study-summary/
// quiz jobs.
func wordSaveHandler(ident identity.Identifier, words wordreview.Store, pipe *pipeline.Pipeline, wordVerifyQueue *asyncjob.Queue) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, ok := requireUser(w, r, ident)
		if !ok {
			return
		}
		var body struct {
			Word    string `json:"word"`
			Meaning string `json:"meaning"`
			Example string `json:"example"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		word := strings.TrimSpace(body.Word)
		meaning := strings.TrimSpace(body.Meaning)
		example := strings.TrimSpace(body.Example)
		if word == "" {
			http.Error(w, "word is required", http.StatusBadRequest)
			return
		}
		if utf8.RuneCountInString(word) > maxWordLen {
			http.Error(w, "word is too long", http.StatusBadRequest)
			return
		}
		if utf8.RuneCountInString(meaning) > maxWordFieldLen || utf8.RuneCountInString(example) > maxWordFieldLen {
			http.Error(w, "meaning/example is too long", http.StatusBadRequest)
			return
		}
		saved, err := words.Save(r.Context(), userID, word, meaning, example)
		if err != nil {
			serverError(w, "words: save "+userID, err)
			return
		}
		if saved.Status == wordreview.StatusPending {
			enqueueOrRunInline(wordVerifyQueue, r.Context(),
				"words: enqueue verify "+userID+"/"+saved.ID,
				func(ctx context.Context) error {
					return transport.EnqueueWordVerifyJob(ctx, wordVerifyQueue, pipe, words, userID, saved.ID)
				},
				"words: verify "+userID+"/"+saved.ID,
				func(ctx context.Context) error {
					return transport.RunWordVerifyInline(ctx, pipe, words, userID, saved.ID)
				},
			)
		}
		writeJSON(w, toWordItem(saved))
	}
}

// wordsListHandler returns the caller's full study list plus how many of
// those words are due for review right now — one call answers both the
// word-review page's list view and the menu badge's due count, so the
// frontend doesn't need two round trips (see fetchWords in
// apps/web/src/lib/wordReview.ts).
func wordsListHandler(ident identity.Identifier, words wordreview.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, ok := requireUser(w, r, ident)
		if !ok {
			return
		}
		now := time.Now()
		list, err := words.List(r.Context(), userID)
		if err != nil {
			serverError(w, "words: list "+userID, err)
			return
		}
		dueCount, err := words.DueCount(r.Context(), userID, now)
		if err != nil {
			serverError(w, "words: due count "+userID, err)
			return
		}
		out := make([]wordItem, len(list))
		for i, word := range list {
			out[i] = toWordItem(word)
		}
		writeJSON(w, map[string]any{"words": out, "dueCount": dueCount})
	}
}

// wordReviewHandler records one review answer against a tracked word,
// advancing or resetting its spaced-repetition schedule (see
// wordreview.nextSchedule), and returns the updated item so the frontend can
// show the learner when it'll come back around.
func wordReviewHandler(ident identity.Identifier, words wordreview.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, ok := requireUser(w, r, ident)
		if !ok {
			return
		}
		var body struct {
			Correct bool `json:"correct"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		updated, err := words.Review(r.Context(), userID, r.PathValue("id"), body.Correct, time.Now())
		if err != nil {
			serverError(w, "words: review "+userID, err)
			return
		}
		if updated.ID == "" {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, toWordItem(updated))
	}
}

// wordDeleteHandler removes one tracked word from the caller's study list.
func wordDeleteHandler(ident identity.Identifier, words wordreview.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, ok := requireUser(w, r, ident)
		if !ok {
			return
		}
		if err := words.Delete(r.Context(), userID, r.PathValue("id")); err != nil {
			serverError(w, "words: delete "+userID, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}
