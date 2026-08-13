package httpserver

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"buddy/server/internal/asyncjob"
	"buddy/server/internal/identity"
	"buddy/server/internal/newsarticle"
	"buddy/server/internal/pipeline"
	"buddy/server/internal/protocol"
	"buddy/server/internal/store"
	"buddy/server/internal/transport"
	"buddy/server/internal/wordlookup"
	"buddy/server/internal/wordreview"
	"github.com/redis/go-redis/v9"
)

// maxWordLen/maxWordFieldLen cap what a learner can persist via
// wordSaveHandler — generous enough for a phrase or idiom (not just a single
// word, see wordreview's package doc) and its meaning/example, same
// abuse-guard reasoning as wordSuggestHandler's maxWordQueryLen.
const (
	maxWordLen            = 255 // matches buddy_word_reviews.word's VARCHAR(255)
	maxWordFieldLen       = 2000
	maxWordLookupPosition = 100000
)

type articleWordLookupResponse struct {
	Status string                   `json:"status"`
	Result *protocol.WordSuggestion `json:"result,omitempty"`
}

// articleWordDefineHandler only accepts the article instance ID from the
// client. The article text is loaded from the caller's own stored instance,
// which prevents a client from mixing a word with a different article's
// passage. A cache hit completes immediately; a miss is durably queued and
// returns while the LLM runs in the background.
func articleWordDefineHandler(ident identity.Identifier, articles newsarticle.Store, pipe *pipeline.Pipeline, rdb redis.UniversalClient, queue *asyncjob.Queue) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, ok := requireUser(w, r, ident)
		if !ok {
			return
		}
		if rdb == nil {
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
		if result, found, err := wordlookup.Get(r.Context(), rdb, key); err != nil {
			serverError(w, "word lookup: cache read", err)
			return
		} else if found {
			writeJSON(w, articleWordLookupResponse{Status: "done", Result: &result})
			return
		}
		if body.CheckOnly {
			writeJSON(w, articleWordLookupResponse{Status: "missing"})
			return
		}
		transport.StartWordDefine(queue, pipe, rdb, lookup)
		writeJSON(w, articleWordLookupResponse{Status: "pending"})
	}
}

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
	OriginalWord string `json:"originalWord"`
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
		Word:         wordreview.NormalizeWord(w.Word),
		Meaning:      w.Meaning,
		Example:      w.Example,
		OriginalWord: w.OriginalWord,
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
// asyncjob.EnqueueOrRunInline pattern transport.FinalizeSession uses for the
// study-summary/quiz jobs.
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
		saved, err := transport.SaveWordAndVerify(r.Context(), words, pipe, wordVerifyQueue, userID, word, meaning, example, strings.TrimSpace(body.OriginalWord))
		if err != nil {
			serverError(w, "words: save "+userID, err)
			return
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
// show the learner when it'll come back around. Repeat marks a
// correct-but-forced-guess answer (the "억지로 맞췄어요" button — see
// WordReview.tsx's markForced) so the word gets rescheduled at the same
// interval instead of advancing; meaningless (and ignored by
// nextSchedule) when Correct is false.
func wordReviewHandler(ident identity.Identifier, words wordreview.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, ok := requireUser(w, r, ident)
		if !ok {
			return
		}
		var body struct {
			Correct bool `json:"correct"`
			Repeat  bool `json:"repeat"`
		}
		if !decodeJSON(w, r, &body) {
			return
		}
		updated, err := words.Review(r.Context(), userID, r.PathValue("id"), body.Correct, body.Repeat, time.Now())
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

func wordResearchHandler(ident identity.Identifier, words wordreview.Store, pipe *pipeline.Pipeline) http.HandlerFunc {
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
		if target.Status != wordreview.StatusRejected && target.OriginalWord != "" {
			http.Error(w, "only excluded or legacy words can be researched", http.StatusConflict)
			return
		}
		word := target.OriginalWord
		if word == "" {
			word = target.Word
		}
		results, err := pipe.DefineWordMeanings(r.Context(), word, target.Example)
		if err != nil {
			serverError(w, "words: research", err)
			return
		}
		writeJSON(w, map[string]any{"suggestions": results})
	}
}

// maxWordQueryLen caps the Korean description a learner can send to
// wordSuggestHandler — generous for the "short phrase describing a word"
// framing wordSuggestionSystemPrompt asks for, same reasoning as
// maxInterlocutorStyleLen guarding against an arbitrarily long paste.
const maxWordQueryLen = 200

// wordSuggestHandler asks pipeline.SuggestWords for English word/phrase
// candidates matching a learner's native-language description — a quick,
// synchronous call (unlike GenerateStudySummary/GenerateStudyQuiz's
// asyncjob-backed handlers) since it's cheap, latency-sensitive, and has
// nothing worth persisting durably: a dropped request just gets retried by
// the learner reopening the panel.
func wordSuggestHandler(ident identity.Identifier, pipe *pipeline.Pipeline) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_, ok := requireUser(w, r, ident)
		if !ok {
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

// maxWordDefineContextLen caps the passage a learner's tapped word came
// from, same abuse-guard reasoning as maxWordQueryLen — generous enough for
// an article's full study paragraph (see ArticleQuiz.tsx), which is the only
// caller today.
const maxWordDefineContextLen = 2000

// wordDefineHandler asks pipeline.DefineWord to define one English
// word/phrase the learner already knows exactly — tapped in an article's
// reading view (see ArticleQuiz.tsx) — rather than described vaguely in
// their native language (that's wordSuggestHandler). Synchronous, same
// latency/nothing-durable reasoning as wordSuggestHandler; returns a single
// WordSuggestion (not wrapped in a list) since there's exactly one word to
// define. The frontend saves it via the existing POST /api/words/save, same
// as a wordSuggestHandler result — this endpoint only looks a word up, it
// never persists anything itself.
func wordDefineHandler(ident identity.Identifier, pipe *pipeline.Pipeline) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_, ok := requireUser(w, r, ident)
		if !ok {
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

// wordAutoAddStatus is what wordAutoAddHandler/wordAutoAddStatusHandler
// return — mirrors articleDraw's "come back immediately, poll for the
// result" shape. Status is store.JobStatusPending/Done/Failed, or "" if no
// run has ever started for this learner (treated as idle, the same as
// Done — see store.Store.GetWordAutoAddStatus). Count is only meaningful
// once Status is JobStatusDone: how many words that run actually added (0
// means the model found nothing new, or everything it suggested failed
// validation) — WordReview.tsx uses it to tell that apart from an error.
type wordAutoAddStatus struct {
	Status string `json:"status"`
	Count  int    `json:"count"`
}

// wordAutoAddHandler starts (or, if one is already running, just reports)
// the "새 단어 추가로 학습하기" background job — the button WordReview.tsx
// shows once the learner's review queue is empty (dueCount 0), replacing
// "복습 시작" in that same slot. The response comes back the instant the
// job is durably marked JobStatusPending (see store.Store.
// StartWordAutoAdd) — it never waits on pipe.SuggestNewWords or the
// per-word VerifyWord checks that follow (see transport.
// EnqueueWordAutoAddJob), so this survives the learner navigating away
// before generation finishes; WordReview.tsx polls
// wordAutoAddStatusHandler until Status leaves JobStatusPending, the same
// way lib/articles.ts's fetchArticleInstance does for "새 아티클 뽑기".
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
		// Already in flight (this learner double-tapped the button, or
		// reopened the page while a previous run was still going) — report
		// it rather than starting a second, redundant generation.
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

// wordAutoAddStatusHandler is wordAutoAddHandler's poll target — the same
// role articleInstanceHandler plays for articleDrawHandler's background
// generation, so a learner who navigates away mid-generation and comes back
// can resume watching it finish (see WordReview.tsx).
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
