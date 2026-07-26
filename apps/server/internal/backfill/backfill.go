// Package backfill asynchronously fills in results that never landed for a
// chat turn — the native-language translation, or (see CorrectionQueue below)
// a grammar-correction result for a turn whose CorrectionStatus is entirely
// empty (saved before that feature existed, or produced by the no-Redis
// inline path — see store.Turn's doc comment). Both kinds never run inline
// with a learner's live conversation: sessions are queued when viewed (see
// httpserver.sessionDetailHandler) and drained by asyncjob.Worker
// (asyncjob.KindTranslation / asyncjob.KindCorrectionBackfill), so this work
// never competes with or delays the interactive pipeline. Durability,
// concurrent claiming across replicas, and crash recovery all come from
// internal/asyncjob — this package only supplies the session-level job shape
// and the handlers that do the actual translating/correcting.
package backfill

import (
	"context"
	"encoding/json"
	"log"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"buddy/server/internal/asyncjob"
	"buddy/server/internal/llm"
	"buddy/server/internal/pipeline"
	"buddy/server/internal/protocol"
	"buddy/server/internal/store"
)

// claimTTL bounds how long a session's translation pass may run before
// asyncjob's reaper treats its worker as dead and hands the session to
// another worker to retry. Translating one session is normally a handful
// of LLM calls (seconds), so this is generous headroom for a slow pass, not
// a tight budget — it only exists to recover from a replica that died
// mid-translation (killed, OOM, deploy).
const claimTTL = 10 * time.Minute

// job is one session queued for backfill — the same (userID, sessionID)
// shape for both translation (Queue/Worker below) and correction
// (CorrectionQueue/CorrectionWorker), since each kind's own asyncjob.Kind
// keyspace already keeps the two from ever mixing up a payload.
type job struct {
	UserID    string `json:"userId"`
	SessionID string `json:"sessionId"`
}

func (j job) dedupeKey() string { return j.UserID + ":" + j.SessionID }

// Queue enqueues sessions that have at least one turn missing its
// translation, onto the shared asyncjob.KindTranslation queue — every
// server replica shares one queue and one dedupe set via Redis. A nil
// *Queue (Redis not configured — see config.Config.RedisClusterHost) makes
// Enqueue a no-op, the same "optional feature, falls through to doing
// nothing" convention as internal/identity's Redis-backed OIDC cache.
type Queue struct {
	q *asyncjob.Queue
}

func NewQueue(rdb redis.UniversalClient) *Queue {
	return &Queue{q: asyncjob.NewQueue(rdb)}
}

// Enqueue adds (userID, sessionID) to the backfill queue, unless it's
// already queued or currently being processed. Best-effort and
// asynchronous by design: an error just leaves this session's translations
// missing until its next view retries, the same as any other
// optional-cache failure in this codebase; callers should invoke this via
// `go` so a slow/unavailable Redis never delays the response that
// triggered it (see httpserver.sessionDetailHandler).
func (q *Queue) Enqueue(ctx context.Context, userID, sessionID string) {
	if q == nil {
		return
	}
	enqueueSession(ctx, q.q, asyncjob.KindTranslation, userID, sessionID, "")
}

// enqueueSession is Queue.Enqueue/CorrectionQueue.Enqueue's shared body —
// the two only differ in which asyncjob.Kind they enqueue onto and the log
// label that identifies which one failed.
func enqueueSession(ctx context.Context, q *asyncjob.Queue, kind asyncjob.Kind, userID, sessionID, logLabel string) {
	j := job{UserID: userID, SessionID: sessionID}
	if _, _, err := q.Enqueue(ctx, kind, j.dedupeKey(), j); err != nil {
		log.Printf("backfill: enqueue %s%s: %v", logLabel, j.dedupeKey(), err)
	}
}

// Worker drains the translation queue, translating every turn missing a
// translation in each queued session — one session, one turn at a time.
// Runs with concurrency 1 (matching pipeline.Pipeline.translationSem's
// process-wide one-call-at-a-time cap on the translation LLM), so this one
// replica never runs two translation passes at once; other replicas each
// run their own Worker the same way, so the cluster as a whole can
// translate multiple sessions concurrently — unlike the single
// cluster-wide-locked drainer this package used before adopting
// internal/asyncjob.
type Worker struct {
	w *asyncjob.Worker
}

func NewWorker(rdb redis.UniversalClient, st store.Store, pipe *pipeline.Pipeline) *Worker {
	return &Worker{w: newSessionWorker(rdb, asyncjob.KindTranslation, st, pipe, "", translateSession)}
}

// newSessionWorker is Worker/CorrectionWorker's shared constructor body:
// both drain a session-keyed queue one job at a time, unmarshal the same
// job payload shape, and hand off to a per-session work function — they
// only differ in which asyncjob.Kind they drain and which function does the
// actual translating/correcting.
func newSessionWorker(rdb redis.UniversalClient, kind asyncjob.Kind, st store.Store, pipe *pipeline.Pipeline, badPayloadLabel string, work func(ctx context.Context, st store.Store, pipe *pipeline.Pipeline, userID, sessionID string)) *asyncjob.Worker {
	handler := func(ctx context.Context, j asyncjob.Job) error {
		var payload job
		if err := json.Unmarshal(j.Payload, &payload); err != nil {
			log.Printf("backfill: bad %sjob payload %q: %v", badPayloadLabel, j.Payload, err)
			return nil // unparseable; retrying it would never succeed
		}
		work(ctx, st, pipe, payload.UserID, payload.SessionID)
		return nil
	}
	return asyncjob.NewWorker(rdb, kind, 1, claimTTL, handler)
}

// Run polls for work until ctx is canceled. Start it with `go worker.Run(ctx)`;
// it never blocks its caller. A nil Worker (Redis not configured) is a
// no-op — see asyncjob.Worker.Run.
func (w *Worker) Run(ctx context.Context) {
	if w == nil {
		return
	}
	w.w.Run(ctx)
}

// translateSession fills in every turn in (userID, sessionID) missing a
// translation, feeding each one every turn before it (verbatim, original
// text, translated or not) as context via pipeline.Pipeline.TranslateWithContext
// — the same "conversation so far" shape correct() uses live — so a
// re-translated turn reads the same as if it had been translated the moment
// it was said. Best-effort per turn: one failed or empty translation is
// logged and skipped, not retried within this pass — the turn is simply
// picked up again the next time its session is viewed and re-queued (see
// httpserver.sessionDetailHandler).
func translateSession(ctx context.Context, st store.Store, pipe *pipeline.Pipeline, userID, sessionID string) {
	turns, ok := loadSessionTurns(ctx, st, userID, sessionID)
	if !ok {
		return
	}
	forEachNonBlankTurn(turns, func(priorTurns []llm.Message, t store.Turn, text string) {
		if strings.TrimSpace(t.Translation) != "" {
			return
		}
		translation, err := pipe.TranslateWithContext(ctx, priorTurns, text)
		if err != nil {
			log.Printf("backfill: translate %s/%s turn %d/%s: %v", userID, sessionID, t.Turn, t.Role, err)
		} else if translation != "" {
			if err := st.SaveTranslation(ctx, userID, sessionID, t.Turn, t.Role, translation); err != nil {
				log.Printf("backfill: save translation %s/%s turn %d/%s: %v", userID, sessionID, t.Turn, t.Role, err)
			}
		}
	})
}

// loadSessionTurns fetches (userID, sessionID)'s transcript for a backfill
// pass, logging and reporting !ok on failure — the shared first step of
// translateSession and correctSession.
func loadSessionTurns(ctx context.Context, st store.Store, userID, sessionID string) ([]store.Turn, bool) {
	_, turns, err := st.SessionDetail(ctx, userID, sessionID)
	if err != nil {
		log.Printf("backfill: session detail %s/%s: %v", userID, sessionID, err)
		return nil, false
	}
	return turns, true
}

// forEachNonBlankTurn walks turns in order, skipping blank ones, and calls
// work with each turn's trimmed text plus every non-blank turn before it as
// llm.Message context — the "conversation so far" shape both
// translateSession and correctSession feed to their respective pipeline
// call.
func forEachNonBlankTurn(turns []store.Turn, work func(priorTurns []llm.Message, t store.Turn, text string)) {
	var priorTurns []llm.Message
	for _, t := range turns {
		text := strings.TrimSpace(t.Text)
		if text == "" {
			continue
		}
		work(priorTurns, t, text)
		priorTurns = append(priorTurns, llm.Message{Role: t.Role, Content: text})
	}
}

// CorrectionQueue enqueues sessions that have at least one user turn missing
// a grammar-correction result entirely, onto the shared
// asyncjob.KindCorrectionBackfill queue — CorrectionQueue/CorrectionWorker's
// equivalent of Queue/Worker above; see the package doc and
// asyncjob.KindCorrectionBackfill for why this is a separate kind from the
// live, turn-level asyncjob.KindCorrection job.
type CorrectionQueue struct {
	q *asyncjob.Queue
}

func NewCorrectionQueue(rdb redis.UniversalClient) *CorrectionQueue {
	return &CorrectionQueue{q: asyncjob.NewQueue(rdb)}
}

// Enqueue mirrors Queue.Enqueue's reasoning (best-effort, dedupe on already
// queued/in-flight, safe on a nil *CorrectionQueue when Redis isn't
// configured, meant to be called via `go` from a request handler).
func (q *CorrectionQueue) Enqueue(ctx context.Context, userID, sessionID string) {
	if q == nil {
		return
	}
	enqueueSession(ctx, q.q, asyncjob.KindCorrectionBackfill, userID, sessionID, "correction ")
}

// CorrectionWorker drains the correction-backfill queue, mirroring Worker's
// reasoning for translation — one session, one turn at a time, concurrency 1
// per replica (this work isn't latency-sensitive, and keeping it modest
// avoids contending with the live analysis ensemble for the same LLM
// backend).
type CorrectionWorker struct {
	w *asyncjob.Worker
}

func NewCorrectionWorker(rdb redis.UniversalClient, st store.Store, pipe *pipeline.Pipeline) *CorrectionWorker {
	return &CorrectionWorker{w: newSessionWorker(rdb, asyncjob.KindCorrectionBackfill, st, pipe, "correction ", correctSession)}
}

// Run mirrors Worker.Run — see that doc comment.
func (w *CorrectionWorker) Run(ctx context.Context) {
	if w == nil {
		return
	}
	w.w.Run(ctx)
}

// correctSession fills in a grammar-correction result for every user turn in
// (userID, sessionID) whose CorrectionStatus is entirely empty and which has
// no Correction yet — see store.Turn's doc comment: this is the "saved
// before this feature existed" case, or a live pass that ran without a
// CorrectHook configured and left nothing durable behind, as opposed to
// CorrectionStatus == "pending"/"processing"/"failed", which the live job's
// own asyncjob reaper already retries on its own (see
// asyncjob.KindCorrectionBackfill's doc comment). Feeds each turn every turn
// before it (verbatim, original text) as context via
// pipeline.Pipeline.CorrectWithContext, the same "conversation so far" shape
// correct() uses live, so a backfilled correction reads the same as if it had
// been generated at the time. Best-effort per turn: one failed correction is
// logged and skipped, not retried within this pass — the turn is simply
// picked up again the next time its session is viewed and re-queued (see
// httpserver.sessionDetailHandler).
func correctSession(ctx context.Context, st store.Store, pipe *pipeline.Pipeline, userID, sessionID string) {
	turns, ok := loadSessionTurns(ctx, st, userID, sessionID)
	if !ok {
		return
	}
	forEachNonBlankTurn(turns, func(priorTurns []llm.Message, t store.Turn, text string) {
		if t.Role != "user" || t.Correction != nil || t.CorrectionStatus != "" {
			return
		}
		corrected, issues, translation, err := pipe.CorrectWithContext(ctx, priorTurns, text)
		if err != nil {
			log.Printf("backfill: correct %s/%s turn %d: %v", userID, sessionID, t.Turn, err)
			return
		}
		if err := st.SaveCorrection(ctx, userID, sessionID, t.Turn, protocol.Correction{
			Original: text, Corrected: corrected, Issues: issues,
		}); err != nil {
			log.Printf("backfill: save correction %s/%s turn %d: %v", userID, sessionID, t.Turn, err)
		}
		if strings.TrimSpace(translation) != "" && strings.TrimSpace(t.Translation) == "" {
			if err := st.SaveTranslation(ctx, userID, sessionID, t.Turn, "user", translation); err != nil {
				log.Printf("backfill: save translation %s/%s turn %d: %v", userID, sessionID, t.Turn, err)
			}
		}
	})
}
