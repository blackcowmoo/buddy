// Package backfill asynchronously fills in the native-language translation
// for chat turns that never got one — e.g. turns saved before the
// translation feature existed, or ones whose original async translation
// call failed. It never runs inline with a learner's live conversation:
// sessions are queued when viewed (see httpserver.sessionDetailHandler) and
// drained by asyncjob.Worker (asyncjob.KindTranslation), so translation
// never competes with or delays the interactive pipeline. Durability,
// concurrent claiming across replicas, and crash recovery all come from
// internal/asyncjob — this package only supplies the translation-specific
// job shape and the handler that does the actual translating.
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
	"buddy/server/internal/store"
)

// claimTTL bounds how long a session's translation pass may run before
// asyncjob's reaper treats its worker as dead and hands the session to
// another worker to retry. Translating one session is normally a handful
// of LLM calls (seconds), so this is generous headroom for a slow pass, not
// a tight budget — it only exists to recover from a replica that died
// mid-translation (killed, OOM, deploy).
const claimTTL = 10 * time.Minute

// job is one session queued for translation backfill.
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
	j := job{UserID: userID, SessionID: sessionID}
	if _, _, err := q.q.Enqueue(ctx, asyncjob.KindTranslation, j.dedupeKey(), j); err != nil {
		log.Printf("backfill: enqueue %s: %v", j.dedupeKey(), err)
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
	handler := func(ctx context.Context, j asyncjob.Job) error {
		var payload job
		if err := json.Unmarshal(j.Payload, &payload); err != nil {
			log.Printf("backfill: bad job payload %q: %v", j.Payload, err)
			return nil // unparseable; retrying it would never succeed
		}
		translateSession(ctx, st, pipe, payload.UserID, payload.SessionID)
		return nil
	}
	return &Worker{w: asyncjob.NewWorker(rdb, asyncjob.KindTranslation, 1, claimTTL, handler)}
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
	_, turns, err := st.SessionDetail(ctx, userID, sessionID)
	if err != nil {
		log.Printf("backfill: session detail %s/%s: %v", userID, sessionID, err)
		return
	}

	var priorTurns []llm.Message
	for _, t := range turns {
		text := strings.TrimSpace(t.Text)
		if text == "" {
			continue
		}
		if strings.TrimSpace(t.Translation) == "" {
			translation, err := pipe.TranslateWithContext(ctx, priorTurns, text)
			if err != nil {
				log.Printf("backfill: translate %s/%s turn %d/%s: %v", userID, sessionID, t.Turn, t.Role, err)
			} else if translation != "" {
				if err := st.SaveTranslation(ctx, userID, sessionID, t.Turn, t.Role, translation); err != nil {
					log.Printf("backfill: save translation %s/%s turn %d/%s: %v", userID, sessionID, t.Turn, t.Role, err)
				}
			}
		}
		priorTurns = append(priorTurns, llm.Message{Role: t.Role, Content: text})
	}
}
