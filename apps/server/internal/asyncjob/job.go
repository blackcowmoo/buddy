// Package asyncjob is a generalized, durable, Redis-backed job queue shared
// by every background task in the server (chat replies, grammar correction,
// translation, title generation, compaction). It generalizes the primitives
// internal/backfill pioneered for translation backfill, fixing that
// package's one limitation for this broader use: backfill serializes all
// work behind a single cluster-wide lock (fine for a low-volume,
// non-interactive job), whereas live chat work needs many jobs — across
// many replicas — running at once.
//
// A job survives both the learner's connection disconnecting (it's not
// tied to any request context — see Worker.run) and the container
// processing it dying outright (crash, OOM, redeploy — see Worker.reapOnce):
// another Worker, on this replica or another, detects the abandoned claim
// and reruns the job from scratch. There is no way to resume a
// partially-streamed LLM generation mid-token, so "from scratch" is the
// deliberate, accepted retry semantics — see Handler's doc comment.
package asyncjob

import "encoding/json"

// Kind identifies what a Job's Payload contains and which Handler processes
// it. Each Kind gets its own Redis queue/processing/dedupe/claim keyspace
// (see queueKey and friends in queue.go), so a burst of one kind's work
// never head-of-line-blocks another, and each kind's Worker can be sized to
// its own urgency independently — e.g. many chat replies in flight at once,
// but translation capped at one call at a time.
type Kind string

const (
	KindReply      Kind = "reply"
	KindCorrection Kind = "correction"
	// KindTranslation is internal/backfill's session-level batch job
	// (payload: {UserID, SessionID} — see backfill.job) — re-translating
	// every turn in a session that's missing one, triggered when a session
	// is viewed. KindLiveTranslation is the separate, turn-level job for a
	// single just-produced assistant reply (payload: {UserID, SessionID,
	// Turn, Text}). The two deliberately don't share a Kind even though
	// both ultimately call the same translation LLM: their payload shapes
	// differ, and one handler misinterpreting the other's payload would
	// silently corrupt data (e.g. translating an empty string into turn 0)
	// rather than erroring — not worth the risk to save one Redis queue.
	KindTranslation     Kind = "translation"
	KindLiveTranslation Kind = "translation-live"
	// KindCorrectionBackfill mirrors KindTranslation's split from
	// KindLiveTranslation, but for grammar correction: it's
	// internal/backfill's session-level batch job (payload: {UserID,
	// SessionID}, same shape as KindTranslation's) that fills in a result for
	// every user turn missing one entirely — CorrectionStatus == "" (see
	// store.Turn's doc comment) — as opposed to KindCorrection, the turn-level
	// live job created by a real conversation turn, whose own reaper already
	// retries a failed attempt. A turn can only reach CorrectionStatus == ""
	// by never having had a KindCorrection job reserved for it in the first
	// place (saved before this feature existed, or produced by the
	// no-Redis/no-hook inline path), so nothing else would ever retry it.
	KindCorrectionBackfill Kind = "correction-backfill"
	KindTitle              Kind = "title"
	KindCompaction         Kind = "compaction"
	// KindStudySummary is the session-level end-of-conversation wrap-up job
	// (payload: {UserID, SessionID} — see transport.EnqueueStudySummaryJob),
	// enqueued once the learner confirms "end this conversation" freezes the
	// room (store.Store.EndSession). Deliberately queued rather than run
	// inline in that HTTP request: an LLM call tied to the request's own
	// context would be lost if the connection dropped before it finished
	// (e.g. the learner navigating away right after confirming), whereas
	// this Kind's context.Background()-scoped job keeps generating the
	// wrap-up regardless — see store.SessionMeta.StudySummaryStatus, the
	// persisted state a reopened room (or the room list) polls to show
	// whether it's still in progress.
	KindStudySummary Kind = "study-summary"
	// KindStudyQuiz is the session-level practice-quiz pre-generation job
	// (payload: {UserID, SessionID} — see transport.EnqueueStudyQuizJob),
	// enqueued alongside KindStudySummary right when EndSession freezes the
	// room, so opening the quiz later (see httpserver.sessionQuizHandler)
	// reads an already-finished result instead of paying for the LLM call in
	// that request. Draws from the exact same flagged issues KindStudySummary
	// does, but runs as its own independent job/status column (store.
	// SessionMeta.QuizStatus) rather than chained after the summary, so a
	// slow or failed summary never delays or blocks the quiz.
	KindStudyQuiz Kind = "study-quiz"
)

// Job is the durable Redis envelope for one unit of background work.
// Payload is Kind-specific JSON — see each Kind's producer/Handler pair
// (e.g. internal/pipeline's ReplyPayload for KindReply) for its shape.
type Job struct {
	ID         string          `json:"id"`
	Kind       Kind            `json:"kind"`
	DedupeKey  string          `json:"dedupeKey"`
	Payload    json.RawMessage `json:"payload"`
	EnqueuedAt int64           `json:"enqueuedAt"`
	// Attempts counts how many times this job has been claimed — 0 the
	// first time, incremented each time reapOnce recovers it from a dead
	// worker. Observability only; Handlers don't need to consult it since
	// every attempt is expected to redo the full job.
	Attempts int `json:"attempts"`
}
