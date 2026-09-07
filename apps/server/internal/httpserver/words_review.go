package httpserver

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"buddy/server/internal/asyncjob"
	"buddy/server/internal/identity"
	"buddy/server/internal/pipeline"
	"buddy/server/internal/transport"
	"buddy/server/internal/wordreview"
)

const (
	maxWordLen      = 255 // buddy_word_reviews.word is VARCHAR(255)
	maxWordFieldLen = 2000
)

// wordItem is the frontend projection of a tracked word. Legacy or partially
// generated questions stay hidden until they satisfy the current contract.
type wordItem struct {
	ID              string                          `json:"id"`
	Word            string                          `json:"word"`
	Meaning         string                          `json:"meaning"`
	Example         string                          `json:"example"`
	OriginalWord    string                          `json:"originalWord"`
	Stage           int                             `json:"stage"`
	ReviewCount     int                             `json:"reviewCount"`
	NextReviewAt    int64                           `json:"nextReviewAt"`
	LastReviewedAt  int64                           `json:"lastReviewedAt,omitempty"`
	Status          string                          `json:"status"`
	VerifyReason    string                          `json:"verifyReason,omitempty"`
	ResearchStatus  string                          `json:"researchStatus,omitempty"`
	ResearchResults []wordreview.ResearchSuggestion `json:"researchResults,omitempty"`
	ReviewQuestion  *wordreview.Question            `json:"reviewQuestion,omitempty"`
}

func toWordItem(word wordreview.Word) wordItem {
	var lastReviewedAt int64
	if !word.LastReviewedAt.IsZero() {
		lastReviewedAt = word.LastReviewedAt.Unix()
	}
	item := wordItem{
		ID:              word.ID,
		Word:            wordreview.NormalizeWord(word.Word),
		Meaning:         word.Meaning,
		Example:         word.Example,
		OriginalWord:    word.OriginalWord,
		Stage:           word.Stage,
		ReviewCount:     word.ReviewCount,
		NextReviewAt:    word.NextReviewAt.Unix(),
		LastReviewedAt:  lastReviewedAt,
		Status:          word.Status,
		VerifyReason:    word.VerifyReason,
		ResearchStatus:  word.ResearchStatus,
		ResearchResults: word.ResearchResults,
	}
	if wordreview.QuestionReady(word) {
		question := word.ReviewQuestion
		item.ReviewQuestion = &question
	}
	return item
}

// wordSaveHandler persists one explicit learner choice immediately, then
// starts verification without holding the request open.
func wordSaveHandler(ident identity.Identifier, words wordreview.Store, pipe *pipeline.Pipeline, wordVerifyQueue *asyncjob.Queue) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, ok := requireUser(w, r, ident)
		if !ok {
			return
		}
		var body struct {
			Word         string `json:"word"`
			Meaning      string `json:"meaning"`
			Example      string `json:"example"`
			OriginalWord string `json:"originalWord"`
		}
		if !decodeJSON(w, r, &body) {
			return
		}
		word := strings.TrimSpace(body.Word)
		meaning := strings.TrimSpace(body.Meaning)
		example := strings.TrimSpace(body.Example)
		if word == "" {
			http.Error(w, "word is required", http.StatusBadRequest)
			return
		}
		if !requireMaxRunes(w, word, maxWordLen, "word is too long") {
			return
		}
		if utf8.RuneCountInString(meaning) > maxWordFieldLen || utf8.RuneCountInString(example) > maxWordFieldLen {
			http.Error(w, "meaning/example is too long", http.StatusBadRequest)
			return
		}
		saved, err := transport.SaveWordAndVerify(
			r.Context(), words, pipe, wordVerifyQueue, userID,
			word, meaning, example, strings.TrimSpace(body.OriginalWord),
		)
		if err != nil {
			serverError(w, "words: save "+userID, err)
			return
		}
		writeJSON(w, toWordItem(saved))
	}
}

// wordsListHandler returns the study list and due count in one round trip. It
// also lazily backfills review questions on verified legacy rows.
func wordsListHandler(ident identity.Identifier, words wordreview.Store, pipe *pipeline.Pipeline, wordVerifyQueue *asyncjob.Queue) http.HandlerFunc {
	// Redis deduplicates across replicas. The map provides the same one-run-per-
	// word property inside a no-Redis process while the page polls.
	var inlineBackfills sync.Map
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
		if _, ok := words.(wordreview.QuestionStore); ok {
			for _, tracked := range list {
				if tracked.Status != wordreview.StatusVerified || wordreview.QuestionReady(tracked) {
					continue
				}
				wordID := tracked.ID
				if wordVerifyQueue == nil {
					key := fmt.Sprintf("%s/%s:q%d", userID, wordID, wordreview.CurrentQuestionVersion)
					if _, running := inlineBackfills.LoadOrStore(key, struct{}{}); running {
						continue
					}
					go func() {
						defer inlineBackfills.Delete(key)
						if err := transport.RunWordVerifyInline(context.Background(), pipe, words, userID, wordID); err != nil {
							log.Printf("words: review-question backfill %s/%s: %v", userID, wordID, err)
						}
					}()
					continue
				}
				asyncjob.EnqueueOrRunInline(wordVerifyQueue, r.Context(),
					"words: enqueue review-question backfill "+userID+"/"+wordID,
					func(ctx context.Context) error {
						return transport.EnqueueWordVerifyJob(ctx, wordVerifyQueue, pipe, words, userID, wordID)
					},
					"words: review-question backfill "+userID+"/"+wordID,
					func(ctx context.Context) error {
						return transport.RunWordVerifyInline(ctx, pipe, words, userID, wordID)
					},
				)
			}
		}
		out := make([]wordItem, len(list))
		for i, word := range list {
			out[i] = toWordItem(word)
		}
		writeJSON(w, map[string]any{"words": out, "dueCount": dueCount})
	}
}

// wordReviewHandler advances or resets one word's spaced-repetition schedule.
func wordReviewHandler(ident identity.Identifier, words wordreview.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, ok := requireUser(w, r, ident)
		if !ok {
			return
		}
		var body struct {
			Correct         bool `json:"correct"`
			Repeat          bool `json:"repeat"`
			QuestionVersion int  `json:"questionVersion"`
		}
		if !decodeJSON(w, r, &body) {
			return
		}
		var updated wordreview.Word
		var err error
		if versioned, ok := words.(wordreview.VersionedReviewer); ok {
			updated, err = versioned.ReviewVersioned(
				r.Context(), userID, r.PathValue("id"), body.QuestionVersion,
				body.Correct, body.Repeat, time.Now(),
			)
		} else {
			updated, err = words.Review(r.Context(), userID, r.PathValue("id"), body.Correct, body.Repeat, time.Now())
		}
		if errors.Is(err, wordreview.ErrQuestionVersion) {
			http.Error(w, "review question is stale", http.StatusConflict)
			return
		}
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
