package transport

import (
	"context"
	"fmt"
	"log"
	"sync"

	"buddy/server/internal/asyncjob"
	"buddy/server/internal/pipeline"
	"buddy/server/internal/store"
)

// FinalizeSession permanently marks one chat room read-only
// (store.Store.EndSession) and kicks off its study-summary/quiz generation —
// the single code path shared by httpserver.sessionEndHandler (the learner
// confirming "종료" via EndConversationControl in apps/web/src/App.tsx) and
// CorrectionJobHandler's own instant-conversation auto-finalize
// (maybeFinalizeInstantSession, triggered server-side the moment a session
// marked instant/"오늘의 한 문장" gets its one real exchange's correction
// back, independent of whether the learner's browser tab/connection is even
// still around to trigger it). Both callers get identical behavior: an
// immediate freeze, then EnqueueStudySummaryJob/EnqueueStudyQuizJob run
// independently rather than chained, via asyncjob.EnqueueOrRunInline so the
// room is safely read-only, and the caller can return, before either job's
// LLM call has even started — the wrap-up itself happens in the background
// from there (see runStudySummary/runStudyQuiz), surviving the caller
// returning (an HTTP response, or this job handler's own completion) the
// same way every other asyncjob-backed job does.
func FinalizeSession(ctx context.Context, st store.Store, pipe *pipeline.Pipeline, studySummaryQueue, studyQuizQueue *asyncjob.Queue, userID, sessionID string) error {
	if err := st.EndSession(ctx, userID, sessionID); err != nil {
		return err
	}

	// Kicked off concurrently, not sequentially: both are independent, and
	// each blocks on its own Enqueue round trip (see
	// asyncjob.EnqueueOrRunInline), so running them one after another would
	// pay that latency twice for no reason.
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		asyncjob.EnqueueOrRunInline(studySummaryQueue, ctx,
			fmt.Sprintf("finalize session: enqueue study summary %s/%s", userID, sessionID),
			func(ctx context.Context) error {
				return EnqueueStudySummaryJob(ctx, studySummaryQueue, pipe, st, userID, sessionID)
			},
			fmt.Sprintf("finalize session: study summary %s/%s", userID, sessionID),
			func(ctx context.Context) error {
				return RunStudySummaryInline(ctx, pipe, st, userID, sessionID)
			},
		)
	}()
	go func() {
		defer wg.Done()
		asyncjob.EnqueueOrRunInline(studyQuizQueue, ctx,
			fmt.Sprintf("finalize session: enqueue study quiz %s/%s", userID, sessionID),
			func(ctx context.Context) error {
				return EnqueueStudyQuizJob(ctx, studyQuizQueue, pipe, st, userID, sessionID)
			},
			fmt.Sprintf("finalize session: study quiz %s/%s", userID, sessionID),
			func(ctx context.Context) error {
				return RunStudyQuizInline(ctx, pipe, st, userID, sessionID)
			},
		)
	}()
	wg.Wait()

	return nil
}

// maybeFinalizeInstantSession auto-finalizes an instant/"오늘의 한 문장" room
// (see store.MySQLStore.MarkInstant) the moment its one real exchange's
// grammar correction lands, so a room like this closes itself out even if
// the learner's browser tab closed or its connection dropped before that —
// unlike apps/web/src/App.tsx's own client-side auto-end effect (still in
// place, and harmless to race against this: EndSession is a plain UPDATE,
// and both EnqueueStudySummaryJob/EnqueueStudyQuizJob dedupe), this doesn't
// depend on a live connection at all, called from CorrectionJobHandler,
// which runs regardless of deployment mode or which replica claims the job.
//
// Only ever fires once per session: turn 0 in this app is always the
// server's own opening greeting (an assistant turn, so it never gets a
// correction job in the first place), so the first — and, since an instant
// room is meant for exactly one exchange, normally only — correction to
// land here is what should close the room. The !meta.Ended guard keeps a
// second correction (e.g. a learner who kept typing anyway) from re-running
// FinalizeSession against an already-finalized room.
func maybeFinalizeInstantSession(ctx context.Context, pipe *pipeline.Pipeline, st store.Store, studySummaryQueue, studyQuizQueue *asyncjob.Queue, userID, sessionID string) {
	meta, _, err := st.SessionDetail(ctx, userID, sessionID)
	if err != nil {
		log.Printf("correction job: auto-finalize instant session: session detail %s/%s: %v", userID, sessionID, err)
		return
	}
	if !meta.Instant || meta.Ended {
		return
	}
	if err := FinalizeSession(ctx, st, pipe, studySummaryQueue, studyQuizQueue, userID, sessionID); err != nil {
		log.Printf("correction job: auto-finalize instant session %s/%s: %v", userID, sessionID, err)
	}
}
