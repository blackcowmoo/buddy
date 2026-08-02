package transport

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"buddy/server/internal/asyncjob"
	"buddy/server/internal/llm"
	"buddy/server/internal/pipeline"
	"buddy/server/internal/protocol"
	"buddy/server/internal/store"
	"buddy/server/internal/wordreview"
)

const (
	// CorrectionClaimTTL/CorrectionWorkerConcurrency mirror ReplyClaimTTL/
	// ReplyWorkerConcurrency's reasoning (see reply_job.go), sized for
	// grammar-analysis calls rather than the chat model but kept just as
	// comfortably above llm.OpenAI's own request timeout, for the same
	// reason: a shorter TTL would reap and duplicate a call that's still
	// legitimately running against a slow local model.
	CorrectionClaimTTL          = 25 * time.Hour
	CorrectionWorkerConcurrency = 8

	// LiveTranslationClaimTTL bounds one turn's translation call — same
	// margin-above-the-LLM-timeout reasoning as ReplyClaimTTL.
	// LiveTranslationWorkerConcurrency stays at 1 to match
	// pipeline.Pipeline.translationSem's process-wide one-call-at-a-time
	// cap on the translation LLM (see pipeline.acquireTranslationSlot,
	// which every AnalyzeTranslation call — direct or via this queue —
	// still goes through).
	LiveTranslationClaimTTL          = 25 * time.Hour
	LiveTranslationWorkerConcurrency = 1

	// TitleClaimTTL/TitleWorkerConcurrency: title generation is a single
	// Complete() call (see pipeline.Pipeline.GenerateTitle) — fast against a
	// hosted API, but no faster than any other call against a slow local
	// model, so TitleClaimTTL needs the same margin above llm.OpenAI's
	// request timeout as the other job kinds. Concurrency stays modest since
	// there's still only ever one title job in flight per room at a time.
	TitleClaimTTL          = 25 * time.Hour
	TitleWorkerConcurrency = 4

	// TitleRegenerateEveryNTurns bounds how often a room's title is
	// refreshed as the conversation keeps going, beyond the initial turn-1
	// generation (see ws.go's emit trigger) — frequent enough that a topic
	// shift shows up before long, infrequent enough that a long
	// conversation doesn't spend one extra LLM call per turn on something
	// decorative.
	TitleRegenerateEveryNTurns = 5
)

// ---- correction ---------------------------------------------------------

type correctionJobPayload struct {
	UserID, SessionID string
	Turn              int
	Text, ContextMsg  string
}

// CorrectionJobHandler builds the asyncjob.Handler that runs one queued
// grammar-correction job: analyzes the sentence and persists the result,
// independent of any connection or its context — see ReplyJobHandler's doc
// comment for the shared durability rationale. Unlike a reply, a repeated
// correction naturally overwrites the same row with an equivalent result
// (store.SaveCorrection is a plain UPDATE), so no done-status guard is
// needed for idempotency here.
//
// An analyze() error is recorded via st.FailJob before the error is
// returned — the reaper still retries the job from scratch regardless (see
// asyncjob.Queue.Execute), but this way a poller (or a page reload) sees
// CorrectionStatus == JobStatusFailed in the meantime instead of a job that
// looks like it's simply still pending forever.
//
// words/wordVerifyQueue feed captureCorrectionWords (see word_capture.go),
// run right after the correction itself is durably saved — this is the one
// call site guaranteed to run for every correction regardless of deployment
// mode (a reap-retry or a different replica's worker has no live connection
// to emit onResult to, but still lands here), so it's where auto-capture
// has to live to never miss a correction. words may be nil in tests that
// don't care about word capture.
func CorrectionJobHandler(pipe *pipeline.Pipeline, st store.Store, words wordreview.Store, wordVerifyQueue *asyncjob.Queue, onResult func(corrected string, issues []protocol.Issue, translation string)) asyncjob.Handler {
	return func(ctx context.Context, job asyncjob.Job) error {
		var payload correctionJobPayload
		if err := json.Unmarshal(job.Payload, &payload); err != nil {
			return fmt.Errorf("correction job: bad payload: %w", err)
		}
		corrected, issues, translation, err := pipe.AnalyzeCorrection(ctx, payload.Text, payload.ContextMsg)
		if err != nil {
			if failErr := st.FailJob(ctx, payload.UserID, payload.SessionID, payload.Turn, "correction", err.Error()); failErr != nil {
				log.Printf("correction job: fail %s/%s#%d: %v", payload.UserID, payload.SessionID, payload.Turn, failErr)
			}
			return fmt.Errorf("correction job: analyze: %w", err)
		}
		result := protocol.Correction{Original: payload.Text, Corrected: corrected, Issues: issues}
		if err := st.SaveCorrection(ctx, payload.UserID, payload.SessionID, payload.Turn, result); err != nil {
			return fmt.Errorf("correction job: save: %w", err)
		}
		captureCorrectionWords(ctx, pipe, words, wordVerifyQueue, payload.UserID, result)
		if strings.TrimSpace(translation) != "" {
			if err := st.SaveTranslation(ctx, payload.UserID, payload.SessionID, payload.Turn, "user", translation); err != nil {
				log.Printf("correction job: save translation %s/%s#%d: %v", payload.UserID, payload.SessionID, payload.Turn, err)
			}
		}
		if onResult != nil {
			onResult(corrected, issues, translation)
		}
		return nil
	}
}

// NewCorrectHook builds the pipeline.CorrectHook that makes grammar
// correction durable — see NewReplyHook's doc comment for the shared
// rationale and the "returns nil without Redis" convention. There is no
// poll-fallback here (unlike NewReplyHook): a correction that finishes on a
// different replica than the learner's own has no live connection to
// stream to anyway, and the frontend's existing pollMissingFeedback (see
// apps/web/src/App.tsx) already re-fetches and picks up a correction that
// landed after the fact.
//
// onFailure fires on every error path below (enqueue, inline claim, inline
// execute) so a connection that's still open never just sees its grammar
// spinner hang — CorrectionJobHandler's own st.FailJob call is what makes
// the failure durable for a poller/reload; this is only about the live
// signal. A dedup ("!ok") or lost-race ("!claimed") return deliberately
// skips onFailure: another attempt already owns this job and will report
// its own outcome.
func NewCorrectHook(pipe *pipeline.Pipeline, st store.Store, words wordreview.Store, wordVerifyQueue *asyncjob.Queue, queue *asyncjob.Queue) pipeline.CorrectHook {
	if queue == nil {
		return nil
	}
	return func(ctx context.Context, userID, sessionID string, turn int, text, contextMsg string, onResult func(string, []protocol.Issue, string), onFailure func()) {
		if err := st.ReserveCorrectionJob(context.Background(), userID, sessionID, turn); err != nil {
			log.Printf("correct: reserve %s/%s#%d: %v", userID, sessionID, turn, err)
		}
		payload := correctionJobPayload{UserID: userID, SessionID: sessionID, Turn: turn, Text: text, ContextMsg: contextMsg}
		logID := turnLogID(userID, sessionID, turn)
		handler := CorrectionJobHandler(pipe, st, words, wordVerifyQueue, onResult)
		// A dedup or lost-race return (ran=false, err=nil) deliberately
		// skips onFailure: another attempt already owns this job and will
		// report its own outcome. An enqueue/claim/execute error (err != nil)
		// always fires it, so a connection that's still open never just
		// sees its grammar spinner hang — CorrectionJobHandler's own
		// st.FailJob call is what makes the failure durable for a
		// poller/reload; this is only about the live signal.
		if _, err := queue.EnqueueAndTryRun(context.Background(), asyncjob.KindCorrection, turnKey(userID, sessionID, turn), logID, payload, CorrectionClaimTTL, handler); err != nil {
			onFailure()
		}
	}
}

// ---- live (per-turn) translation -----------------------------------------

type liveTranslationJobPayload struct {
	UserID, SessionID string
	Turn              int
	Role              string
	Text              string
}

func liveTranslationDedupeKey(userID, sessionID string, turn int, role string) string {
	return turnKey(userID, sessionID, turn) + ":" + role
}

// TranslationJobHandler builds the asyncjob.Handler that runs one queued
// live-translation job — see CorrectionJobHandler's doc comment for the
// shared idempotency/durability reasoning (store.SaveTranslation is also a
// plain UPDATE).
func TranslationJobHandler(pipe *pipeline.Pipeline, st store.Store, onResult func(translation string)) asyncjob.Handler {
	return func(ctx context.Context, job asyncjob.Job) error {
		var payload liveTranslationJobPayload
		if err := json.Unmarshal(job.Payload, &payload); err != nil {
			return fmt.Errorf("translation job: bad payload: %w", err)
		}
		translation, err := pipe.AnalyzeTranslation(ctx, payload.Text)
		if err != nil {
			return fmt.Errorf("translation job: analyze: %w", err)
		}
		if err := st.SaveTranslation(ctx, payload.UserID, payload.SessionID, payload.Turn, payload.Role, translation); err != nil {
			return fmt.Errorf("translation job: save: %w", err)
		}
		if onResult != nil {
			onResult(translation)
		}
		return nil
	}
}

// NewTranslateHook builds the pipeline.TranslateHook that makes live
// per-turn translation durable — see NewCorrectHook's doc comment for why
// there's no poll-fallback here either.
func NewTranslateHook(pipe *pipeline.Pipeline, st store.Store, queue *asyncjob.Queue) pipeline.TranslateHook {
	if queue == nil {
		return nil
	}
	return func(ctx context.Context, userID, sessionID string, turn int, text string, onResult func(string)) {
		payload := liveTranslationJobPayload{UserID: userID, SessionID: sessionID, Turn: turn, Role: "assistant", Text: text}
		logID := turnLogID(userID, sessionID, turn)
		dedupeKey := liveTranslationDedupeKey(userID, sessionID, turn, "assistant")
		queue.EnqueueAndTryRun(context.Background(), asyncjob.KindLiveTranslation, dedupeKey, logID, payload, LiveTranslationClaimTTL, TranslationJobHandler(pipe, st, onResult))
	}
}

// ---- title generation -----------------------------------------------------

type titleJobPayload struct {
	UserID, SessionID string
	Turn              int
	Transcript        []llm.Message
}

// TitleJobHandler builds the asyncjob.Handler that runs one queued
// title-generation job. store.SaveGeneratedTitle now overwrites on every
// call (see its doc comment), so a repeat call for the same turn — e.g. from
// a reap-retry — just regenerates an equivalent title rather than being a
// no-op; that's fine, same idempotency reasoning as CorrectionJobHandler.
func TitleJobHandler(pipe *pipeline.Pipeline, st store.Store) asyncjob.Handler {
	return func(ctx context.Context, job asyncjob.Job) error {
		var payload titleJobPayload
		if err := json.Unmarshal(job.Payload, &payload); err != nil {
			return fmt.Errorf("title job: bad payload: %w", err)
		}
		title, err := pipe.GenerateTitle(ctx, payload.Transcript)
		if err != nil {
			return fmt.Errorf("title job: generate: %w", err)
		}
		if title = strings.TrimSpace(title); title == "" {
			return nil
		}
		if err := st.SaveGeneratedTitle(ctx, payload.UserID, payload.SessionID, title); err != nil {
			return fmt.Errorf("title job: save: %w", err)
		}
		return nil
	}
}

// NewTitleHook builds the pipeline.TitleHook that makes title generation
// durable — see NewCorrectHook's doc comment for why there's no
// poll-fallback/onResult here either (a title landing after the fact has no
// live-connection UX to serve, it just shows up next time the room list is
// fetched — see TitleJobHandler, which persists on its own).
func NewTitleHook(pipe *pipeline.Pipeline, st store.Store, queue *asyncjob.Queue) pipeline.TitleHook {
	if queue == nil {
		return nil
	}
	return func(ctx context.Context, userID, sessionID string, turn int, transcript []llm.Message) {
		// context.Background(), not ctx, for the same reason as
		// NewReplyHook/NewCorrectHook/NewTranslateHook: this must keep
		// making progress even if the connection that triggered it
		// disconnects right after this point.
		payload := titleJobPayload{UserID: userID, SessionID: sessionID, Turn: turn, Transcript: transcript}
		logID := turnLogID(userID, sessionID, turn)
		// turnKey includes turn, unlike the userID:sessionID-only key this
		// used when a title was only ever generated once per room: without
		// turn, every regeneration attempt (see TitleRegenerateEveryNTurns)
		// would collide in the asyncjob dedupe/claim keyspace with the
		// room's very first title job, silently dropping every later
		// regeneration.
		queue.EnqueueAndTryRun(context.Background(), asyncjob.KindTitle, turnKey(userID, sessionID, turn), logID, payload, TitleClaimTTL, TitleJobHandler(pipe, st))
	}
}
