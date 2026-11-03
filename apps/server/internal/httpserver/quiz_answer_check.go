package httpserver

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"

	"buddy/server/internal/asyncjob"
	"buddy/server/internal/identity"
	"buddy/server/internal/pipeline"
	"buddy/server/internal/quizcache"
	"buddy/server/internal/transport"
	"buddy/server/internal/workguard"
)

// maxQuizAnswerCheckLen caps the typed answer quizAnswerCheckHandler sends to
// pipeline.CheckQuizAnswer — a fill-in-the-blank answer is at most a short
// phrase, same "cap a free-text field a learner controls" reasoning as
// maxWordQueryLen.
const maxQuizAnswerCheckLen = 200

type quizOwners struct{ Sessions, Words any }

// quizAnswerCheckHandler grades synonyms the client cannot match locally.
// Owner guards stop grading deleted sessions/words, including on cache hits.
// Verdicts are Redis-only (quizcache): a nil store (no Redis) disables reuse
// and refinement entirely, and a degraded Redis degrades to a miss rather
// than failing the check — the verdict is regenerable from Chat, so caching
// is never worth a 500 here.
func quizAnswerCheckHandler(ident identity.Identifier, pipe *pipeline.Pipeline, owners quizOwners, cache quizcache.Store, refineQueue *asyncjob.Queue) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, ok := requireUser(w, r, ident)
		if !ok {
			return
		}
		var body struct {
			SessionID         string   `json:"sessionId"`
			WordID            string   `json:"wordId"`
			Prompt            string   `json:"prompt"`
			Answer            string   `json:"answer"`
			AcceptableAnswers []string `json:"acceptableAnswers"`
			LearnerAnswer     string   `json:"learnerAnswer"`
		}
		if !decodeJSON(w, r, &body) {
			return
		}
		prompt := strings.TrimSpace(body.Prompt)
		answer := strings.TrimSpace(body.Answer)
		learnerAnswer := strings.TrimSpace(body.LearnerAnswer)
		if prompt == "" || answer == "" || learnerAnswer == "" {
			http.Error(w, "prompt, answer, and learnerAnswer are required", http.StatusBadRequest)
			return
		}
		if !requireMaxRunes(w, learnerAnswer, maxQuizAnswerCheckLen, fmt.Sprintf("learnerAnswer exceeds %d characters", maxQuizAnswerCheckLen)) {
			return
		}
		ctx := r.Context()
		if body.SessionID != "" {
			ctx = workguard.BindStore(ctx, owners.Sessions, userID, body.SessionID)
		}
		if body.WordID != "" {
			ctx = workguard.BindStore(ctx, owners.Words, userID, body.WordID)
		}
		if err := workguard.Check(ctx); err != nil {
			http.NotFound(w, r)
			return
		}
		key := quizcache.Key(prompt, answer, learnerAnswer)
		if cache != nil {
			if cached, found := cache.Lookup(ctx, key); found {
				if !cached.Correct {
					// A wrong verdict keeps getting re-hit because the learner keeps
					// answering this word wrong; renewing its TTL keeps exactly the
					// verdicts still under active review warm while one-off wrong
					// answers expire out of the cache on their own.
					cache.Renew(ctx, key)
				}
				writeJSON(w, map[string]any{"correct": cached.Correct, "similar": cached.Similar})
				return
			}
		}
		correct, chatDraft, err := pipe.CheckQuizAnswerFast(ctx, prompt, answer, body.AcceptableAnswers, learnerAnswer)
		if err != nil {
			if errors.Is(err, workguard.ErrDeleted) {
				http.NotFound(w, r)
				return
			}
			serverError(w, "check quiz answer", err)
			return
		}
		writeJSON(w, map[string]any{"correct": correct, "similar": correct})
		// The learner already has Chat's verdict; complete the Analysis ->
		// Judge cascade durably so the next equivalent answer is a cache hit.
		// No inline fallback when the queue is nil: refinement exists only to
		// fill the Redis cache, and no queue means no cache (see
		// asyncjob.KindQuizAnswerRefine).
		if refineQueue != nil {
			payload := transport.QuizAnswerRefinePayload{
				UserID:            userID,
				SessionID:         body.SessionID,
				WordID:            body.WordID,
				Prompt:            prompt,
				Answer:            answer,
				AcceptableAnswers: body.AcceptableAnswers,
				LearnerAnswer:     learnerAnswer,
				ChatDraft:         chatDraft,
			}
			if err := transport.EnqueueQuizAnswerRefine(ctx, refineQueue, pipe, cache, owners.Sessions, owners.Words, payload); err != nil {
				log.Printf("enqueue quiz answer refine: %v", err)
			}
		}
	}
}
