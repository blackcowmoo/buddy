package httpserver

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"buddy/server/internal/identity"
	"buddy/server/internal/pipeline"
	"buddy/server/internal/wordreview"
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
// A nil cache disables reuse and background refinement.
func quizAnswerCheckHandler(ident identity.Identifier, pipe *pipeline.Pipeline, owners quizOwners, cache wordreview.AnswerCache) http.HandlerFunc {
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
		if cache != nil {
			if result, found, err := cache.LookupAnswer(r.Context(), prompt, answer, learnerAnswer, time.Now()); err != nil {
				serverError(w, "lookup quiz answer cache", err)
				return
			} else if found {
				writeJSON(w, map[string]any{"correct": result, "similar": result})
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
		if cache != nil {
			// Preserve the verdict already shown in this attempt, but improve the
			// durable cache for the next equivalent answer. Background context is
			// intentional: the HTTP request must finish after Chat, and closing the
			// page must not cancel Analysis/Judge halfway through.
			go func(acceptableAnswers []string) {
				refined, err := pipe.RefineQuizAnswerFromDraft(context.WithoutCancel(ctx), prompt, answer, acceptableAnswers, learnerAnswer, chatDraft)
				if err != nil {
					log.Printf("refine quiz answer: %v", err)
					return
				}
				if err := cache.SaveAnswer(context.WithoutCancel(ctx), prompt, answer, learnerAnswer, refined, time.Now()); err != nil {
					log.Printf("save refined quiz answer cache: %v", err)
				}
			}(append([]string(nil), body.AcceptableAnswers...))
		}
	}
}
