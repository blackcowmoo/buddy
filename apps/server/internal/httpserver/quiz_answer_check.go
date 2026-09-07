package httpserver

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"buddy/server/internal/identity"
	"buddy/server/internal/pipeline"
	"buddy/server/internal/wordreview"
)

// maxQuizAnswerCheckLen caps the typed answer quizAnswerCheckHandler sends to
// pipeline.CheckQuizAnswer — a fill-in-the-blank answer is at most a short
// phrase, same "cap a free-text field a learner controls" reasoning as
// maxWordQueryLen.
const maxQuizAnswerCheckLen = 200

// quizAnswerCheckHandler asks pipeline.CheckQuizAnswer whether a learner's
// typed quiz answer should count as correct, for the one case QuizPanel's
// client-side isQuizAnswerAccepted can't already resolve on its own: an
// answer that didn't literally match QuizQuestion.answer or
// acceptableAnswers, but might still be a genuine synonym the model didn't
// think to list at quiz-generation time. Not session-scoped — the question
// itself (prompt/answer/acceptableAnswers) is already loaded client-side
// (see App.tsx's endedQuiz), so this only needs what's in the request body,
// the same "no session lookup needed" shape as wordSuggestHandler.
func quizAnswerCheckHandler(ident identity.Identifier, pipe *pipeline.Pipeline, caches ...wordreview.AnswerCache) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_, ok := requireUser(w, r, ident)
		if !ok {
			return
		}
		var body struct {
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
		var cache wordreview.AnswerCache
		if len(caches) > 0 {
			cache = caches[0]
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
		correct, chatDraft, err := pipe.CheckQuizAnswerFast(r.Context(), prompt, answer, body.AcceptableAnswers, learnerAnswer)
		if err != nil {
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
				refined, err := pipe.RefineQuizAnswerFromDraft(context.Background(), prompt, answer, acceptableAnswers, learnerAnswer, chatDraft)
				if err != nil {
					log.Printf("refine quiz answer: %v", err)
					return
				}
				if err := cache.SaveAnswer(context.Background(), prompt, answer, learnerAnswer, refined, time.Now()); err != nil {
					log.Printf("save refined quiz answer cache: %v", err)
				}
			}(append([]string(nil), body.AcceptableAnswers...))
		}
	}
}
