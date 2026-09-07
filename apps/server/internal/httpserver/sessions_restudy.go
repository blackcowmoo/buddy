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

// sessionRestudyHandler lets a learner force-regenerate an ended session's
// study-summary wrap-up when it landed as JobStatusDone with an empty
// result — the "다시 확인하기" button EndConversationControl (apps/web/src/
// App.tsx) shows only in that exact state. That combination should be rare
// now that runStudySummary itself treats an empty result as a failure
// (see its doc comment), but a session that already landed there before
// that fix — or hit some other still-unknown gap — has no other way back:
// needsStudySummaryBackfill only re-triggers a JobStatusPending row, and
// this one reads as done. Gated server-side on the same state the button is
// shown for (not just trusting the client) so this can only ever regenerate
// an empty wrap-up, never clobber one that already has real content. A
// session ended before the wrap-up became an async job reads
// StudySummaryStatus as "" rather than JobStatusDone (see the
// store.SessionMeta.StudySummaryStatus doc comment) — the button's own
// gating (and EndConversationControl's rendering) already treats "" the
// same as done, so this check must too, or every one of those legacy
// sessions 409s the instant a learner taps the button.
func sessionRestudyHandler(ident identity.Identifier, st store.Store, pipe *pipeline.Pipeline, studySummaryQueue *asyncjob.Queue) http.HandlerFunc {
	return sessionRestartHandler(ident, st, sessionRestartSpec{
		eligible: func(meta store.SessionMeta) bool {
			done := meta.StudySummaryStatus == store.JobStatusDone || meta.StudySummaryStatus == ""
			return done && len(meta.StudySummary) == 0
		},
		conflict:       "study summary is not in a re-checkable state",
		restartContext: "restart study summary",
		restart:        st.RestartStudySummary,
		dispatch: func(ctx context.Context, userID, sessionID string) {
			asyncjob.EnqueueOrRunInline(studySummaryQueue, ctx,
				fmt.Sprintf("restudy session: enqueue study summary %s/%s", userID, sessionID),
				func(jobCtx context.Context) error {
					return transport.EnqueueStudySummaryJob(jobCtx, studySummaryQueue, pipe, st, userID, sessionID)
				},
				fmt.Sprintf("restudy session: study summary %s/%s", userID, sessionID),
				func(jobCtx context.Context) error {
					return transport.RunStudySummaryInline(jobCtx, pipe, st, userID, sessionID)
				},
			)
		},
	})
}
