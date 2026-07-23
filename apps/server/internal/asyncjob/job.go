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
	KindTitle           Kind = "title"
	KindCompaction      Kind = "compaction"
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
