package httpserver

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"

	"buddy/server/internal/asyncjob"
	"buddy/server/internal/backfill"
	"buddy/server/internal/identity"
	"buddy/server/internal/pipeline"
	"buddy/server/internal/store"
	"buddy/server/internal/transport"
)

// defaultSessionPageLimit is how many distinct turns sessionDetailHandler
// loads when the request doesn't specify ?limit= — enough for most rooms to
// load in one page, small enough that a very long-running room's replay
// doesn't ship its entire history (and every embedded correction/
// translation) on first paint. The frontend requests older pages by turn
// cursor (see ?before=) as the learner scrolls up.
const defaultSessionPageLimit = 30

// sessionDetailHandler returns a page of one session's transcript for
// replay, most recent turns first: ?before= (a turn number cursor — omit or
// 0 for the latest page) and ?limit= (page size — omit or non-positive for
// defaultSessionPageLimit) select the page, and the "hasMore" field in the
// response reports whether older turns exist beyond it. An explicit
// non-positive ?limit= instead requests the whole transcript via the
// unbounded store.SessionDetail, with "hasMore" always false. Either way,
// the store scopes the lookup by the caller's own userID, so a session ID
// belonging to someone else 404s exactly like one that doesn't exist at
// all — this handler can't tell the difference, on purpose.
//
// Viewing a session is also what triggers translation and correction
// backfill: if any turn on the returned page is missing its native-language
// translation (saved before the translation feature existed, or a one-off
// async failure at the time — see internal/backfill's doc comment), the
// session is queued for background re-translation; likewise, if any user
// turn is missing a grammar-correction result entirely — CorrectionStatus ==
// "" (see store.Turn's doc comment) — it's queued for background
// re-correction via asyncjob.KindCorrectionBackfill, since (unlike a
// CorrectionStatus == "failed" turn) nothing else would ever retry it. The
// same goes for a study-summary wrap-up left in JobStatusPending with no
// StudySummary yet — see needsStudySummaryBackfill — which is how the
// legacy-reset migration in store.NewMySQL gets its rows regenerated rather
// than left permanently blank, and for a session ended before quiz
// pre-generation existed at all — see needsStudyQuizBackfill, which
// re-enqueues asyncjob.KindStudyQuiz the first time such a session is
// reopened, so it only ever needs generating once rather than staying stuck
// on the old on-demand-at-click-time path forever. Either way this never
// delays the response: Enqueue is a couple of fast Redis calls, but it's
// still fired via `go` so a slow/unavailable Redis can never make opening a
// conversation wait on it, and the actual work happens entirely out-of-band
// in internal/backfill's Workers (or the pooled asyncjob.KindStudySummary/
// KindStudyQuiz Workers — see cmd/server/main.go), over the session's whole
// transcript regardless of which page triggered it — the learner sees
// today's (possibly still-missing) results immediately and gets the
// filled-in ones on their next visit.
func sessionDetailHandler(ident identity.Identifier, st store.Store, translateQueue *backfill.Queue, correctionQueue *backfill.CorrectionQueue, pipe *pipeline.Pipeline, studySummaryQueue *asyncjob.Queue, studyQuizQueue *asyncjob.Queue) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, ok := requireUser(w, r, ident)
		if !ok {
			return
		}
		sessionID := r.PathValue("id")
		beforeTurn, _ := strconv.Atoi(r.URL.Query().Get("before"))
		// limit is only defaulted when the caller omits it entirely (or sends
		// something unparseable) — an explicit "limit=0" (or negative) is a
		// deliberate request for the whole transcript (routed to the
		// unbounded store.SessionDetail below), which is how
		// pollMissingFeedback (apps/web/src/App.tsx) still polls every turn
		// rather than just the latest page.
		limit := defaultSessionPageLimit
		if v := r.URL.Query().Get("limit"); v != "" {
			if n, err := strconv.Atoi(v); err == nil {
				limit = n
			}
		}
		var meta store.SessionMeta
		var turns []store.Turn
		var hasMore bool
		var err error
		if limit <= 0 {
			meta, turns, err = st.SessionDetail(r.Context(), userID, sessionID)
		} else {
			meta, turns, hasMore, err = st.SessionDetailPage(r.Context(), userID, sessionID, beforeTurn, limit)
		}
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if err != nil {
			serverError(w, "session detail", err)
			return
		}
		if needsTranslationBackfill(turns) {
			go translateQueue.Enqueue(context.Background(), userID, sessionID)
		}
		if needsCorrectionBackfill(turns) {
			go correctionQueue.Enqueue(context.Background(), userID, sessionID)
		}
		if needsStudySummaryBackfill(meta, turns) {
			go func() {
				if err := transport.EnqueueStudySummaryJob(context.Background(), studySummaryQueue, pipe, st, userID, sessionID); err != nil {
					log.Printf("session detail: enqueue study summary backfill %s/%s: %v", userID, sessionID, err)
				}
			}()
		}
		if needsStudyQuizBackfill(meta) {
			go func() {
				if err := transport.EnqueueStudyQuizJob(context.Background(), studyQuizQueue, pipe, st, userID, sessionID); err != nil {
					log.Printf("session detail: enqueue study quiz backfill %s/%s: %v", userID, sessionID, err)
				}
			}()
		}
		writeJSON(w, map[string]any{"session": meta, "turns": turns, "hasMore": hasMore})
	}
}

// needsTranslationBackfill reports whether any non-blank turn in the
// transcript is missing its native-language translation.
func needsTranslationBackfill(turns []store.Turn) bool {
	for _, t := range turns {
		if strings.TrimSpace(t.Text) != "" && strings.TrimSpace(t.Translation) == "" {
			return true
		}
	}
	return false
}

// needsCorrectionBackfill reports whether any non-blank user turn in the
// transcript is missing a grammar-correction result entirely —
// CorrectionStatus == "" and no Correction yet. A turn with CorrectionStatus
// "pending"/"processing"/"failed" is excluded on purpose: those are already
// tracked by a live asyncjob.KindCorrection job, whose own reaper retries a
// failed attempt on its own (see pollMissingFeedback in apps/web/src/App.tsx)
// — only a turn no job was ever reserved for needs this backfill path.
func needsCorrectionBackfill(turns []store.Turn) bool {
	for _, t := range turns {
		if t.Role == "user" && strings.TrimSpace(t.Text) != "" && t.Correction == nil && t.CorrectionStatus == "" {
			return true
		}
	}
	return false
}

// needsStudySummaryBackfill reports whether an ended session's wrap-up needs
// (re)generating: StudySummaryStatus == JobStatusPending with no
// StudySummary yet, but real flagged issues still sitting in the transcript's
// per-turn corrections. That combination only arises from the legacy-reset
// migration in store.NewMySQL, which resets a pre-bilingual "done" wrap-up
// back to pending rather than leaving it stuck — a freshly-ended session
// that's still actually being generated has the exact same status, but
// hasn't had a chance to accumulate a transcript worth flagging issues in
// yet, so gating on CollectStudyIssues here doesn't fight the live job (and
// even if it did, EnqueueStudySummaryJob's dedupe makes a redundant enqueue
// harmless). A session with genuinely nothing to flag never reaches this
// check: len(issues) == 0 short-circuits it.
func needsStudySummaryBackfill(meta store.SessionMeta, turns []store.Turn) bool {
	if !meta.Ended || meta.StudySummaryStatus != store.JobStatusPending || len(meta.StudySummary) != 0 {
		return false
	}
	return len(transport.CollectStudyIssues(turns)) > 0
}

// needsStudyQuizBackfill reports whether an ended session predates quiz
// pre-generation entirely: QuizStatus == "" only ever arises from a session
// that was ended before asyncjob.KindStudyQuiz existed (see EndSession,
// which now sets QuizStatus to JobStatusPending the instant it ends any
// session) — every session ended since then reaches a terminal QuizStatus
// (JobStatusDone, even with an empty quiz — see runStudyQuiz) on its own, so
// there's no live job here for a redundant enqueue to race, unlike
// needsStudySummaryBackfill's legacy-reset case. Doesn't gate on
// CollectStudyIssues the way needsStudySummaryBackfill does: an ended
// session with no flagged issues still needs its QuizStatus moved off ""
// (to JobStatusDone with an empty quiz), or it would look "still pending"
// forever.
func needsStudyQuizBackfill(meta store.SessionMeta) bool {
	return meta.Ended && meta.QuizStatus == ""
}
