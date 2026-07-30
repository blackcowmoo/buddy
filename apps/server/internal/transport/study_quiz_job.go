package transport

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"buddy/server/internal/asyncjob"
	"buddy/server/internal/pipeline"
	"buddy/server/internal/protocol"
	"buddy/server/internal/store"
)

// StudyQuizClaimTTL/StudyQuizWorkerConcurrency mirror StudySummaryClaimTTL/
// StudySummaryWorkerConcurrency's reasoning: one Complete() call against the
// Analysis ensemble (see pipeline.Pipeline.GenerateStudyQuiz), so the same
// margin above llm.OpenAI's own request timeout applies. Concurrency stays
// modest since a learner only ever ends one room at a time.
const (
	StudyQuizClaimTTL          = 25 * time.Hour
	StudyQuizWorkerConcurrency = 4
)

// studyQuizJobPayload mirrors studySummaryJobPayload: just the two IDs, not
// the transcript/issues themselves, so a reap-retry always quizzes whatever
// is actually persisted rather than a payload that could be stale by the
// time it runs.
type studyQuizJobPayload struct {
	UserID    string
	SessionID string
}

// runStudyQuiz is the actual work behind asyncjob.KindStudyQuiz: gather the
// same flagged issues runStudySummary draws from (see CollectStudyIssues),
// synthesize them into a practice quiz unless there's nothing to synthesize,
// and persist the result. Shared by StudyQuizJobHandler (the queued, durable
// path) and RunStudyQuizInline (httpserver.sessionEndHandler's fallback when
// Redis isn't configured) so both paths behave identically. Unlike
// runStudySummary, an empty-but-valid result is accepted as done rather than
// treated as a failure: a quiz's "questions" array being empty is exactly
// how httpserver.sessionQuizHandler already represented "nothing to quiz",
// so there's no ambiguity here for a retry to resolve.
func runStudyQuiz(ctx context.Context, pipe *pipeline.Pipeline, st store.Store, userID, sessionID string) error {
	_, turns, err := st.SessionDetail(ctx, userID, sessionID)
	if err != nil {
		return fmt.Errorf("study quiz: session detail: %w", err)
	}

	issues := CollectStudyIssues(turns)

	var questions []protocol.QuizQuestion
	if len(issues) > 0 {
		questions, err = pipe.GenerateStudyQuiz(ctx, issues)
		if err != nil {
			if failErr := st.FailStudyQuiz(context.Background(), userID, sessionID); failErr != nil {
				log.Printf("study quiz: fail %s/%s: %v", userID, sessionID, failErr)
			}
			return fmt.Errorf("study quiz: generate: %w", err)
		}
	}

	if err := st.CompleteStudyQuiz(ctx, userID, sessionID, questions); err != nil {
		return fmt.Errorf("study quiz: complete: %w", err)
	}
	return nil
}

// StudyQuizJobHandler builds the asyncjob.Handler that runs one queued quiz
// pre-generation job, independent of any connection or its context — see
// StudySummaryJobHandler's doc comment for the shared durability rationale.
func StudyQuizJobHandler(pipe *pipeline.Pipeline, st store.Store) asyncjob.Handler {
	return func(ctx context.Context, job asyncjob.Job) error {
		var payload studyQuizJobPayload
		if err := json.Unmarshal(job.Payload, &payload); err != nil {
			return fmt.Errorf("study quiz job: bad payload: %w", err)
		}
		return runStudyQuiz(ctx, pipe, st, payload.UserID, payload.SessionID)
	}
}

// RunStudyQuizInline runs the exact same work as StudyQuizJobHandler,
// synchronously — mirrors RunStudySummaryInline; the caller is expected to
// run this on a detached context.Background() goroutine of its own for
// "freeze now, keep working after the response" behavior without Redis.
func RunStudyQuizInline(ctx context.Context, pipe *pipeline.Pipeline, st store.Store, userID, sessionID string) error {
	return runStudyQuiz(ctx, pipe, st, userID, sessionID)
}

// EnqueueStudyQuizJob durably queues quiz pre-generation for a just-frozen
// session — mirrors EnqueueStudySummaryJob exactly, including the
// synchronous-enqueue/background-execute split; see its doc comment for the
// full reasoning. Called alongside EnqueueStudySummaryJob (not chained after
// it) from httpserver.sessionEndHandler, since both jobs draw independently
// from the same already-persisted issues.
func EnqueueStudyQuizJob(ctx context.Context, queue *asyncjob.Queue, pipe *pipeline.Pipeline, st store.Store, userID, sessionID string) error {
	payload := studyQuizJobPayload{UserID: userID, SessionID: sessionID}
	job, ok, err := queue.Enqueue(ctx, asyncjob.KindStudyQuiz, turnKey(userID, sessionID, 0), payload)
	if err != nil {
		return fmt.Errorf("study quiz: enqueue: %w", err)
	}
	if !ok {
		return nil // already queued or in flight — another attempt owns it
	}
	logID := turnLogID(userID, sessionID, 0)
	handler := StudyQuizJobHandler(pipe, st)
	go func() {
		claimed, err := queue.TryClaimByID(context.Background(), job, StudyQuizClaimTTL)
		if err != nil {
			log.Printf("study quiz: inline claim %s: %v", logID, err)
			return
		}
		if !claimed {
			return // lost the race to a pooled Worker, which owns it now
		}
		if err := queue.Execute(context.Background(), job, handler); err != nil {
			log.Printf("study quiz: inline execute %s: %v", logID, err)
		}
	}()
	return nil
}
