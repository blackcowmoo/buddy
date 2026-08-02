package wordreview

import (
	"testing"
	"time"
)

func TestNextScheduleAdvancesStageOnCorrectAnswer(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	newStage, nextReviewAt := nextSchedule(0, true, now)

	if newStage != 1 {
		t.Errorf("newStage = %d, want 1", newStage)
	}
	want := now.Add(stageIntervals[0])
	if !nextReviewAt.Equal(want) {
		t.Errorf("nextReviewAt = %v, want %v (stageIntervals[0] out: a first-time correct answer schedules 1 day, not a step further)", nextReviewAt, want)
	}
}

// TestNextScheduleKeepsGrowingPastTheHandTunedStages guards the "no ceiling"
// requirement directly: a word must never stop advancing or get excluded
// from tracking just because it outgrew the hand-tuned stageIntervals table —
// see the table's own doc comment for why a fixed cap would make daily
// review load grow with the size of the whole vocabulary instead of staying
// bounded by how many words are still being learned.
func TestNextScheduleKeepsGrowingPastTheHandTunedStages(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	lastStage := len(stageIntervals) - 1

	newStage, nextReviewAt := nextSchedule(lastStage, true, now)

	wantStage := lastStage + 1
	if newStage != wantStage {
		t.Errorf("newStage = %d, want %d (still advancing, never retires)", newStage, wantStage)
	}
	wantInterval := stageIntervals[lastStage] // the table's last entry, no doubling yet
	want := now.Add(wantInterval)
	if !nextReviewAt.Equal(want) {
		t.Errorf("nextReviewAt = %v, want %v (the table's last entry, indexed by the old stage)", nextReviewAt, want)
	}

	// And it keeps going: several more correct answers in a row keep pushing
	// the interval out further each time, never plateauing.
	stage := newStage
	prevInterval := wantInterval
	for i := 0; i < 5; i++ {
		var next time.Time
		stage, next = nextSchedule(stage, true, now)
		interval := next.Sub(now)
		if interval <= prevInterval {
			t.Fatalf("interval did not keep growing: prev=%v next=%v (stage %d)", prevInterval, interval, stage)
		}
		prevInterval = interval
	}
}

func TestIntervalForStageClampsToMaxIntervalInsteadOfOverflowing(t *testing.T) {
	// A very high stage would overflow time.Duration's int64 nanoseconds if
	// intervalForStage kept doubling without a numeric safety ceiling — this
	// is purely an overflow guard (see maxInterval's doc), not a behavioral
	// cap: it only kicks in at a multi-decade interval, far past anything a
	// review schedule would realistically reach.
	got := intervalForStage(1000)
	if got != maxInterval {
		t.Errorf("intervalForStage(1000) = %v, want the maxInterval clamp %v", got, maxInterval)
	}
}

func TestNextScheduleResetsToStageZeroOnMiss(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	newStage, nextReviewAt := nextSchedule(3, false, now)

	if newStage != 0 {
		t.Errorf("newStage = %d, want 0 (a miss resets progress, not just one step back)", newStage)
	}
	if !nextReviewAt.Equal(now) {
		t.Errorf("nextReviewAt = %v, want %v (due immediately, retryable the same day rather than pushed to tomorrow)", nextReviewAt, now)
	}
}

func TestNextScheduleMissAtStageZeroStaysAtZero(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	newStage, nextReviewAt := nextSchedule(0, false, now)

	if newStage != 0 {
		t.Errorf("newStage = %d, want 0 (never goes negative)", newStage)
	}
	if !nextReviewAt.Equal(now) {
		t.Errorf("nextReviewAt = %v, want %v", nextReviewAt, now)
	}
}
