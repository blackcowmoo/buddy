package transport

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"buddy/server/internal/asyncjob"
	"buddy/server/internal/pipeline"
	"buddy/server/internal/store"
)

// StudySummaryClaimTTL/StudySummaryWorkerConcurrency mirror TitleClaimTTL/
// TitleWorkerConcurrency's reasoning: one Complete() call against the
// Analysis ensemble (see pipeline.Pipeline.GenerateStudySummary), so the
// same margin above llm.OpenAI's own request timeout applies. Concurrency
// stays modest since a learner only ever ends one room at a time.
const (
	StudySummaryClaimTTL          = 25 * time.Hour
	StudySummaryWorkerConcurrency = 4
)

// studySummaryJobPayload is the durable envelope for one queued
// end-of-conversation wrap-up job — deliberately just the two IDs, not the
// transcript/issues themselves: the handler re-reads the session fresh (see
// runStudySummary), so a reap-retry always summarizes whatever's actually
// persisted rather than a payload that could be stale by the time it runs.
type studySummaryJobPayload struct {
	UserID    string
	SessionID string
}

// runStudySummary is the actual work behind asyncjob.KindStudySummary:
// gather every grammar/vocabulary/phrasing/context issue flagged across the
// session's transcript (same aggregation the old, synchronous
// sessionStudySummaryHandler used to do inline), synthesize them into one
// wrap-up unless there's nothing to synthesize, persist the result, and
// best-effort fold it into the learner's cross-session profile. Shared by
// StudySummaryJobHandler (the queued, durable path) and
// RunStudySummaryInline (httpserver.sessionEndHandler's fallback when Redis
// isn't configured) so both paths behave identically.
func runStudySummary(ctx context.Context, pipe *pipeline.Pipeline, st store.Store, userID, sessionID string) error {
	_, turns, err := st.SessionDetail(ctx, userID, sessionID)
	if err != nil {
		return fmt.Errorf("study summary: session detail: %w", err)
	}

	var issues []pipeline.StudyIssue
	for _, t := range turns {
		if t.Role != "user" || t.Correction == nil {
			continue
		}
		for _, iss := range t.Correction.Issues {
			issues = append(issues, pipeline.StudyIssue{Text: t.Text, Issue: iss})
		}
	}

	var summary string
	if len(issues) > 0 {
		summary, err = pipe.GenerateStudySummary(ctx, issues)
		if err != nil {
			if failErr := st.FailStudySummary(context.Background(), userID, sessionID); failErr != nil {
				log.Printf("study summary: fail %s/%s: %v", userID, sessionID, failErr)
			}
			return fmt.Errorf("study summary: generate: %w", err)
		}
	}

	if err := st.CompleteStudySummary(ctx, userID, sessionID, summary); err != nil {
		return fmt.Errorf("study summary: complete: %w", err)
	}

	// Folding into the cross-session profile is best-effort, same reasoning
	// as the old sessionEndHandler: it enriches future conversations, but a
	// transient failure here must not leave this session's own wrap-up
	// stuck — CompleteStudySummary above already landed regardless.
	if summary != "" {
		prevProfile, err := st.GetLearnerProfile(ctx, userID)
		if err != nil {
			log.Printf("study summary: get learner profile %s: %v", userID, err)
		} else if merged, err := pipe.UpdateLearnerProfile(ctx, prevProfile, summary); err != nil {
			log.Printf("study summary: update learner profile %s: %v", userID, err)
		} else if err := st.SaveLearnerProfile(ctx, userID, merged); err != nil {
			log.Printf("study summary: save learner profile %s: %v", userID, err)
		}
	}
	return nil
}

// StudySummaryJobHandler builds the asyncjob.Handler that runs one queued
// end-of-conversation wrap-up job, independent of any connection or its
// context — see ReplyJobHandler's doc comment for the shared durability
// rationale. runStudySummary's own FailStudySummary call is what makes a
// failure durable for a poller/reload in the meantime; the reaper still
// retries the job from scratch regardless (see asyncjob.Queue.Execute).
func StudySummaryJobHandler(pipe *pipeline.Pipeline, st store.Store) asyncjob.Handler {
	return func(ctx context.Context, job asyncjob.Job) error {
		var payload studySummaryJobPayload
		if err := json.Unmarshal(job.Payload, &payload); err != nil {
			return fmt.Errorf("study summary job: bad payload: %w", err)
		}
		return runStudySummary(ctx, pipe, st, payload.UserID, payload.SessionID)
	}
}

// RunStudySummaryInline runs the exact same work as StudySummaryJobHandler,
// synchronously, for httpserver.sessionEndHandler's no-Redis fallback (see
// EnqueueStudySummaryJob's doc comment) — there's no queue to make it
// durable, so the caller is expected to run this on a detached
// context.Background() goroutine of its own if it wants "freeze now, keep
// working after the response" behavior without Redis.
func RunStudySummaryInline(ctx context.Context, pipe *pipeline.Pipeline, st store.Store, userID, sessionID string) error {
	return runStudySummary(ctx, pipe, st, userID, sessionID)
}

// EnqueueStudySummaryJob durably queues the wrap-up generation for a
// just-frozen session (see httpserver.sessionEndHandler, called right after
// store.Store.EndSession). Enqueue itself happens synchronously, on ctx —
// cheap (no LLM call), so it's safe to await before the HTTP response — so
// the job is durably sitting in Redis even if this replica dies moments
// later; only the actual LLM call runs in the background, in a detached
// goroutine racing the pooled asyncjob.Worker (see cmd/server/main.go) to
// claim it, on context.Background() so it isn't cut short by the request
// that triggered it ending. Mirrors Queue.EnqueueAndTryRun's fast path, just
// split across the synchronous/background boundary sessionEndHandler needs.
func EnqueueStudySummaryJob(ctx context.Context, queue *asyncjob.Queue, pipe *pipeline.Pipeline, st store.Store, userID, sessionID string) error {
	payload := studySummaryJobPayload{UserID: userID, SessionID: sessionID}
	job, ok, err := queue.Enqueue(ctx, asyncjob.KindStudySummary, turnKey(userID, sessionID, 0), payload)
	if err != nil {
		return fmt.Errorf("study summary: enqueue: %w", err)
	}
	if !ok {
		return nil // already queued or in flight — another attempt owns it
	}
	logID := turnLogID(userID, sessionID, 0)
	handler := StudySummaryJobHandler(pipe, st)
	go func() {
		claimed, err := queue.TryClaimByID(context.Background(), job, StudySummaryClaimTTL)
		if err != nil {
			log.Printf("study summary: inline claim %s: %v", logID, err)
			return
		}
		if !claimed {
			return // lost the race to a pooled Worker, which owns it now
		}
		if err := queue.Execute(context.Background(), job, handler); err != nil {
			log.Printf("study summary: inline execute %s: %v", logID, err)
		}
	}()
	return nil
}
