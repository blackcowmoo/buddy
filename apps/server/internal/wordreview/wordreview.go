// Package wordreview persists words/phrases the learner is meant to study —
// either ones they explicitly chose from the word-search popup
// (internal/pipeline.SuggestWords, see httpserver.wordSaveHandler), or ones
// auto-captured from a vocabulary/phrasing fix in an in-conversation grammar
// correction (see transport.captureCorrectionWords): a word reached for
// mid-conversation and gotten wrong is just as worth drilling as one the
// learner couldn't produce at all. Either way this package schedules when to
// re-quiz them using a Leitner-style spaced-repetition schedule loosely
// modeled on the Ebbinghaus forgetting curve: each correct recall pushes the
// next review further out; a miss resets progress and makes the word due
// again immediately, so it can be retried the same day instead of waiting
// until the next scheduled interval. This package doesn't generate
// word/meaning/example itself — those come from whichever source captured
// the word — it only tracks review scheduling for whichever words ended up
// saved.
//
// A newly saved word starts StatusPending and enters review rotation only
// once pipeline.Pipeline.VerifyWord's model-consensus check confirms it's a
// real word/phrase with an accurate meaning (see
// transport.WordVerifyJobHandler) — MarkVerified/MarkRejected record that
// outcome. A rejected word is never silently deleted: it stays visible (see
// httpserver.wordsListHandler) so the learner can see why and remove it
// themselves.
package wordreview

import (
	"context"
	"time"
)

// stageIntervals hand-tunes the spacing for a word's first several correct
// recalls — steep at first (matching how fast the forgetting curve drops
// right after learning something new), widening from there. Past the last
// entry, intervalForStage keeps doubling indefinitely instead of capping: a
// fixed ceiling (e.g. "never review more than 30 days apart") would mean a
// growing vocabulary keeps piling more and more well-known words into that
// same 30-day bucket forever, so review load per day grows with the size of
// the whole collection. Letting well-known words drift to 60, 120, 500+ days
// apart — and never fully retiring them — keeps daily load bounded by how
// many words are still *being learned*, not by the total ever added.
var stageIntervals = []time.Duration{
	24 * time.Hour,      // stage 0 -> 1
	2 * 24 * time.Hour,  // stage 1 -> 2
	4 * 24 * time.Hour,  // stage 2 -> 3
	7 * 24 * time.Hour,  // stage 3 -> 4
	15 * 24 * time.Hour, // stage 4 -> 5
	30 * 24 * time.Hour, // stage 5 and beyond -> doubles from here
}

// maxInterval is a pure overflow/sanity guard, not a behavioral cap — a
// word this far apart contributes essentially nothing to any day's review
// load regardless of how large the collection grows, so clamping here never
// causes the pile-up a fixed ceiling would. It exists only because
// time.Duration is an int64 count of nanoseconds and stageIntervals'
// doubling would eventually overflow it.
const maxInterval = 20 * 365 * 24 * time.Hour

// intervalForStage returns how long to wait before the next review at stage.
// Follows the hand-tuned stageIntervals table while stage is within it, then
// keeps doubling the last entry indefinitely — see stageIntervals' doc for
// why there's deliberately no ceiling.
func intervalForStage(stage int) time.Duration {
	if stage < len(stageIntervals) {
		return stageIntervals[stage]
	}
	interval := stageIntervals[len(stageIntervals)-1]
	for i := 0; i < stage-(len(stageIntervals)-1); i++ {
		if interval > maxInterval/2 {
			return maxInterval
		}
		interval *= 2
	}
	return interval
}

// nextSchedule computes one review answer's outcome, given the word's stage
// going in. correct advances a stage and pushes the next review out by
// intervalForStage(stage) — the interval stageIntervals labels for that
// stage->stage+1 step, so a first-ever (or post-miss) correct answer, from
// stage 0, schedules exactly stageIntervals[0] (1 day) out, not a step
// further. incorrect resets to stage 0 and reschedules the word due right
// now instead of a day out — a miss means the forgetting curve reset, not
// just slowed by a step, and the word should be retryable the same day
// rather than pushed to tomorrow.
func nextSchedule(stage int, correct bool, now time.Time) (newStage int, nextReviewAt time.Time) {
	if correct {
		newStage = stage + 1
		return newStage, now.Add(intervalForStage(stage))
	}
	return 0, now
}

// Status values for Word.Status — see the package doc for the
// pending -> verified/rejected lifecycle.
const (
	StatusPending  = "pending"
	StatusVerified = "verified"
	StatusRejected = "rejected"
)

// Word is one word/phrase/idiom the learner chose to study, plus its review
// schedule.
type Word struct {
	ID             string
	UserID         string
	Word           string
	Meaning        string
	Example        string
	Stage          int
	ReviewCount    int
	CorrectStreak  int
	NextReviewAt   time.Time
	LastReviewedAt time.Time // zero value if never reviewed yet
	CreatedAt      time.Time
	// Status is StatusPending/StatusVerified/StatusRejected — see the
	// package doc. Only StatusVerified words are ever due for review (Due/
	// DueCount filter on it); a fresh Save always starts StatusPending.
	Status string
	// VerifyReason is the model-consensus check's explanation for its
	// verdict — set on MarkRejected (surfaced to the learner so they know
	// why), and left "" for a word that's still pending or was verified
	// (nothing to explain about a pass).
	VerifyReason string
}

// Store persists the learner's study words (chosen or auto-captured — see
// the package doc) and their review schedules, scoped per user the same way
// internal/store and internal/recording are.
type Store interface {
	// Save adds word/meaning/example to userID's study list as
	// StatusPending — not yet due for review; see MarkVerified. A no-op that
	// returns the existing row if this exact (word, meaning) pair is already
	// tracked for userID — re-searching (and re-choosing to study) it must
	// never reset progress already made. The same word text with a
	// *different* meaning is a distinct row: "bank" (riverbank) and "bank"
	// (financial) are tracked, verified, and scheduled independently.
	Save(ctx context.Context, userID, word, meaning, example string) (Word, error)
	// Get returns one tracked word by id. A no-op — zero Word, nil error —
	// if id doesn't exist or belongs to a different user, same
	// indistinguishable-from-missing contract as Review/MarkVerified.
	Get(ctx context.Context, userID, id string) (Word, error)
	// List returns userID's tracked words in any status, most recently added
	// first. Every row carries NextReviewAt/Status so the caller can filter
	// down to due-for-review (or pending/rejected) words itself — see
	// httpserver.wordsListHandler, which returns this alongside DueCount in
	// one response rather than requiring a second call.
	List(ctx context.Context, userID string) ([]Word, error)
	// DueCount is how many of userID's StatusVerified words have
	// nextReviewAt in the past — cheap enough to call on every app load for
	// the menu badge. Pending/rejected words are never due.
	DueCount(ctx context.Context, userID string, now time.Time) (int, error)
	// MarkVerified transitions a StatusPending word to StatusVerified once
	// pipeline.Pipeline.VerifyWord's model-consensus check passes it,
	// starting its review clock from now (not from when it was saved, since
	// verification runs in the background and may take a moment) — see
	// nextSchedule/intervalForStage. A no-op — zero Word, nil error — if id
	// doesn't exist or belongs to a different user.
	MarkVerified(ctx context.Context, userID, id string, now time.Time) (Word, error)
	// MarkRejected transitions a StatusPending word to StatusRejected with
	// reason recorded for the learner to see (never auto-deleted — see the
	// package doc). A no-op if id doesn't exist or belongs to a different
	// user.
	MarkRejected(ctx context.Context, userID, id string, reason string) (Word, error)
	// Review applies one review answer's outcome (see nextSchedule) to word
	// id and returns the updated row. A no-op — zero Word, nil error — if id
	// doesn't exist or belongs to a different user.
	Review(ctx context.Context, userID, id string, correct bool, now time.Time) (Word, error)
	// Delete removes one tracked word, any status — used both for an active
	// word and for clearing a rejected/pending one out of the list. A no-op
	// if id doesn't exist or belongs to a different user.
	Delete(ctx context.Context, userID, id string) error
	Close() error
}
