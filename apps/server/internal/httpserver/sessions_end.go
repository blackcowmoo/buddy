package httpserver

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"

	"buddy/server/internal/asyncjob"
	"buddy/server/internal/identity"
	"buddy/server/internal/pipeline"
	"buddy/server/internal/store"
	"buddy/server/internal/transport"
)

// sessionEndHandler permanently marks one chat room read-only
// (store.Store.EndSession) the instant the learner confirms "end this
// conversation" (see EndConversationControl in apps/web/src/App.tsx) —
// freezing never waits on either background job's LLM call, which this
// kicks off separately right after (transport.EnqueueStudySummaryJob and
// transport.EnqueueStudyQuizJob, run independently rather than chained), so
// the room is safely read-only, and the response comes back, before either
// call has even started. The wrap-up — and folding it into the learner's
// persistent cross-session profile — happens in the background from there
// (see transport.StudySummaryJobHandler/runStudySummary), and the practice
// quiz alongside it (see transport.StudyQuizJobHandler/runStudyQuiz), pre-
// generated now instead of on demand so the "퀴즈 풀기" button later reads an
// already-finished result: both survive the learner navigating away right
// after this returns, which is the entire point of routing them through
// asyncjob rather than generating them inline in this request.
func sessionEndHandler(ident identity.Identifier, st store.Store, pipe *pipeline.Pipeline, studySummaryQueue *asyncjob.Queue, studyQuizQueue *asyncjob.Queue) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, ok := requireUser(w, r, ident)
		if !ok {
			return
		}
		sessionID := r.PathValue("id")

		if err := st.EndSession(r.Context(), userID, sessionID); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				http.NotFound(w, r)
				return
			}
			serverError(w, "end session", err)
			return
		}

		// Kicked off concurrently, not sequentially: both are independent, and
		// each blocks the response on its own Enqueue round trip (see
		// enqueueOrRunInline), so running them one after another would pay
		// that latency twice for no reason.
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			enqueueOrRunInline(studySummaryQueue, r.Context(),
				fmt.Sprintf("end session: enqueue study summary %s/%s", userID, sessionID),
				func(ctx context.Context) error {
					return transport.EnqueueStudySummaryJob(ctx, studySummaryQueue, pipe, st, userID, sessionID)
				},
				fmt.Sprintf("end session: study summary %s/%s", userID, sessionID),
				func(ctx context.Context) error {
					return transport.RunStudySummaryInline(ctx, pipe, st, userID, sessionID)
				},
			)
		}()
		go func() {
			defer wg.Done()
			enqueueOrRunInline(studyQuizQueue, r.Context(),
				fmt.Sprintf("end session: enqueue study quiz %s/%s", userID, sessionID),
				func(ctx context.Context) error {
					return transport.EnqueueStudyQuizJob(ctx, studyQuizQueue, pipe, st, userID, sessionID)
				},
				fmt.Sprintf("end session: study quiz %s/%s", userID, sessionID),
				func(ctx context.Context) error {
					return transport.RunStudyQuizInline(ctx, pipe, st, userID, sessionID)
				},
			)
		}()
		wg.Wait()

		w.WriteHeader(http.StatusNoContent)
	}
}
