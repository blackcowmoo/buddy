package httpserver

import (
	"context"
	"fmt"
	"net/http"

	"buddy/server/internal/asyncjob"
	"buddy/server/internal/identity"
	"buddy/server/internal/pipeline"
	"buddy/server/internal/store"
	"buddy/server/internal/transport"
)

// sessionQuizResetHandler lets a learner force-regenerate an ended session's
// practice quiz from scratch — the "퀴즈 다시 만들기" button in
// EndConversationControl (apps/web/src/App.tsx), shown whenever a quiz has
// finished generating (whether or not it came back empty). Unlike
// sessionRestudyHandler, this doesn't require the existing result to be
// empty first: the whole point is regenerating a quiz that already has real
// questions, e.g. one generated before AnswerMeaning/AcceptableAnswers
// existed. Still gated on QuizStatus being terminal (JobStatusDone,
// JobStatusFailed, or "" for a session ended before quiz pre-generation
// existed) rather than JobStatusPending, so this can't race a
// still-in-flight generation into two competing writers.
func sessionQuizResetHandler(ident identity.Identifier, st store.Store, pipe *pipeline.Pipeline, studyQuizQueue *asyncjob.Queue) http.HandlerFunc {
	return sessionRestartHandler(ident, st, sessionRestartSpec{
		eligible: func(meta store.SessionMeta) bool {
			return meta.QuizStatus == store.JobStatusDone ||
				meta.QuizStatus == store.JobStatusFailed || meta.QuizStatus == ""
		},
		conflict:       "quiz is not in a resettable state",
		restartContext: "restart study quiz",
		restart:        st.RestartStudyQuiz,
		dispatch: func(ctx context.Context, userID, sessionID string) {
			asyncjob.EnqueueOrRunInline(studyQuizQueue, ctx,
				fmt.Sprintf("reset quiz: enqueue study quiz %s/%s", userID, sessionID),
				func(jobCtx context.Context) error {
					return transport.EnqueueStudyQuizJob(jobCtx, studyQuizQueue, pipe, st, userID, sessionID)
				},
				fmt.Sprintf("reset quiz: study quiz %s/%s", userID, sessionID),
				func(jobCtx context.Context) error {
					return transport.RunStudyQuizInline(jobCtx, pipe, st, userID, sessionID)
				},
			)
		},
	})
}
