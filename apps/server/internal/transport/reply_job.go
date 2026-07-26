package transport

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"buddy/server/internal/asyncjob"
	"buddy/server/internal/llm"
	"buddy/server/internal/pipeline"
	"buddy/server/internal/store"
)

const (
	// ReplyClaimTTL bounds how long a reply job may run before asyncjob's
	// reaper treats its owner as dead and hands it to another replica to
	// regenerate from scratch — there is no way to resume a
	// partially-streamed LLM generation (see internal/asyncjob's package
	// doc). Kept comfortably above llm.OpenAI's own request timeout: if
	// this were shorter (or even close to it), a still-legitimately-running
	// local-model completion would get reaped and requeued out from under
	// itself, piling duplicate retries onto an already-slow server instead
	// of just waiting for the one in flight to finish.
	ReplyClaimTTL = 25 * time.Hour

	// ReplyWorkerConcurrency is how many reply jobs one replica's
	// background Worker pool runs at once — picking up jobs whose original
	// replica died, or that lost the fast-path inline-claim race. Generous:
	// unlike translation, reply jobs are the user-facing hot path and
	// should never queue behind each other within a replica.
	ReplyWorkerConcurrency = 16

	// replyPollAttempts bounds the fallback poll a connection falls back to
	// when it didn't end up running its own reply job inline (see
	// pollReplyUntilDone) — combined with replyPollInterval, comfortably
	// longer than ReplyClaimTTL plus one retry, so a job reaped once still
	// has time to finish on its second attempt before this connection gives
	// up.
	replyPollAttempts = 60
)

// replyPollInterval is how often pollReplyUntilDone rechecks job status. A
// var, not a const, so tests can shrink it rather than waiting out the
// production interval.
var replyPollInterval = 3 * time.Second

// replyJobPayload is the durable envelope for one queued chat-reply job —
// everything replyJobHandler needs to reproduce the exact call
// reply()/StartConversation() would have made directly, on whichever
// replica ends up running it.
type replyJobPayload struct {
	UserID    string        `json:"userId"`
	SessionID string        `json:"sessionId"`
	Turn      int           `json:"turn"`
	Messages  []llm.Message `json:"messages"`
	Fallback  string        `json:"fallback"`
}

func replyDedupeKey(userID, sessionID string, turn int) string {
	return userID + ":" + sessionID + ":" + strconv.Itoa(turn)
}

// ReplyJobHandler builds the asyncjob.Handler that actually runs a queued
// reply job: idempotent (skips the LLM call if this turn's reply already
// completed — see store.JobStatus) and independent of any specific WS
// connection or its context, which is what lets a reply survive both a
// disconnect and this replica dying mid-generation (the caller decides
// ctx — see NewReplyHook, which always passes context.Background() rather
// than a connection-scoped one). onToken/onDone are best-effort hooks for
// when this exact call is the one streaming to a still-open connection
// (the fast path in NewReplyHook); the background Worker pool (see
// cmd/server/main.go) passes nil for both.
func ReplyJobHandler(pipe *pipeline.Pipeline, st store.Store, onToken func(string), onDone func(string)) asyncjob.Handler {
	return func(ctx context.Context, job asyncjob.Job) error {
		var payload replyJobPayload
		if err := json.Unmarshal(job.Payload, &payload); err != nil {
			return fmt.Errorf("reply job: bad payload: %w", err)
		}
		status, err := st.JobStatus(ctx, payload.UserID, payload.SessionID, payload.Turn, "reply")
		if err != nil {
			return fmt.Errorf("reply job: status: %w", err)
		}
		if status == store.JobStatusDone {
			return nil // a previous attempt already finished this turn
		}
		tok := onToken
		if tok == nil {
			tok = func(string) {}
		}
		full, _ := pipe.GenerateReply(ctx, payload.Messages, payload.Fallback, tok)
		if strings.TrimSpace(full) == "" {
			// GenerateReply already substitutes Fallback (always non-empty)
			// on any LLM error, so reaching here means even that failed to
			// come through — don't persist an empty reply; leave the job
			// for the reaper to retry.
			return fmt.Errorf("reply job: empty reply")
		}
		if err := st.CompleteAssistantTurn(ctx, payload.UserID, payload.SessionID, payload.Turn, full); err != nil {
			return fmt.Errorf("reply job: complete: %w", err)
		}
		if onDone != nil {
			onDone(full)
		}
		return nil
	}
}

// NewReplyHook builds the pipeline.ReplyHook that makes turn replies
// durable: queued in Redis (internal/asyncjob) and persisted in MySQL
// independent of any WS connection's lifetime or this replica's own — see
// ReplyJobHandler. Returns nil (leaving pipeline.Pipeline.ReplyHook unset,
// so reply()/StartConversation fall back to calling the chat model
// directly in-process) when queue is nil, i.e. Redis isn't configured —
// the same "optional feature, zero setup by default" convention as every
// other Redis-backed feature in this codebase.
func NewReplyHook(pipe *pipeline.Pipeline, st store.Store, queue *asyncjob.Queue) pipeline.ReplyHook {
	if queue == nil {
		return nil
	}
	return func(ctx context.Context, userID, sessionID string, turn int, msgs []llm.Message, fallback string, onToken func(string), onDone func(string)) {
		// context.Background(), not ctx, for every store/queue call below:
		// this hook must keep making progress even if the connection that
		// triggered it disconnects or barges in right after this point —
		// that's the entire point of durability here (see internal/asyncjob's
		// package doc). ctx is only still consulted by pollReplyUntilDone,
		// where it just controls when THIS connection stops watching, not
		// whether the reply itself keeps going.
		if err := st.ReserveAssistantTurn(context.Background(), userID, sessionID, turn); err != nil {
			log.Printf("reply: reserve %s/%s#%d: %v", userID, sessionID, turn, err)
		}
		job, ok, err := queue.Enqueue(context.Background(), asyncjob.KindReply, replyDedupeKey(userID, sessionID, turn), replyJobPayload{
			UserID: userID, SessionID: sessionID, Turn: turn, Messages: msgs, Fallback: fallback,
		})
		if err != nil {
			log.Printf("reply: enqueue %s/%s#%d: %v", userID, sessionID, turn, err)
		}
		if !ok {
			// Deduped: a reply job for this exact turn is already queued or
			// in flight (e.g. a reconnect racing the original connection's
			// enqueue, or this same Enqueue call retried after a partial
			// failure above). Poll for its result rather than starting a
			// second LLM call for the same turn.
			pollReplyUntilDone(ctx, st, userID, sessionID, turn, onDone)
			return
		}
		// Fast path: try to run the job this connection just created
		// inline, before any pooled Worker's blocking dequeue gets to it —
		// identical latency to calling the chat model directly, but still
		// durable: Execute leaves the job claimed for the reaper on any
		// error, and the LLM call itself runs on context.Background(), so
		// a disconnect right after this point can't cut it short.
		claimed, err := queue.TryClaimByID(context.Background(), job, ReplyClaimTTL)
		if err != nil {
			log.Printf("reply: inline claim %s/%s#%d: %v", userID, sessionID, turn, err)
		}
		if claimed {
			handler := ReplyJobHandler(pipe, st, onToken, onDone)
			if err := queue.Execute(context.Background(), job, handler); err != nil {
				log.Printf("reply: inline execute %s/%s#%d: %v", userID, sessionID, turn, err)
				// The job is left claimed for the stale-claim reaper to
				// retry (see asyncjob.Worker.run's matching behavior) —
				// this connection still needs to learn the eventual result
				// somehow, so fall back to polling for it.
				pollReplyUntilDone(ctx, st, userID, sessionID, turn, onDone)
			}
			return
		}
		// Lost the race to a pooled Worker (already running this job on
		// this or another replica) — poll until it's done rather than
		// starting a second LLM call ourselves.
		pollReplyUntilDone(ctx, st, userID, sessionID, turn, onDone)
	}
}

// pollReplyUntilDone is the fallback path for when this connection didn't
// end up running the reply job itself. It has no tokens to stream live, so
// onDone only fires once with the finished text, rendered whole rather
// than typed out token-by-token — an accepted, rare degradation versus the
// fast path's identical-to-direct-call UX. ctx is the connection's own
// (turn-scoped) context: if the learner disconnects or barges in while
// this is polling, it simply stops watching — the reply itself keeps going
// independently (see ReplyJobHandler) and is picked up on the learner's
// next reconnect via SessionDetail's per-turn reply status instead.
func pollReplyUntilDone(ctx context.Context, st store.Store, userID, sessionID string, turn int, onDone func(string)) {
	for i := 0; i < replyPollAttempts; i++ {
		select {
		case <-ctx.Done():
			return
		case <-time.After(replyPollInterval):
		}
		status, err := st.JobStatus(context.Background(), userID, sessionID, turn, "reply")
		if err != nil {
			log.Printf("reply: poll status %s/%s#%d: %v", userID, sessionID, turn, err)
			continue
		}
		if status != store.JobStatusDone {
			continue
		}
		_, turns, err := st.SessionDetail(context.Background(), userID, sessionID)
		if err != nil {
			log.Printf("reply: poll detail %s/%s#%d: %v", userID, sessionID, turn, err)
			return
		}
		for _, t := range turns {
			if t.Turn == turn && t.Role == "assistant" {
				onDone(t.Text)
				return
			}
		}
		return
	}
	log.Printf("reply: poll %s/%s#%d: gave up after %d attempts", userID, sessionID, turn, replyPollAttempts)
}
