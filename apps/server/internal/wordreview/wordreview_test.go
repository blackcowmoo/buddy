package wordreview

import (
	"testing"
	"time"
)

func TestNormalizeWordLowercasesVocabularyButPreservesFirstPersonPronouns(t *testing.T) {
	tests := map[string]string{
		"Leverage":       "leverage",
		"Mitigate risks": "mitigate risks",
		"I":              "I",
		"I'M READY":      "I'm ready",
		"  Streamline  ": "streamline",
	}
	for input, want := range tests {
		if got := NormalizeWord(input); got != want {
			t.Errorf("NormalizeWord(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestNextScheduleAdvancesStageOnCorrectAnswer(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	newStage, nextReviewAt := nextSchedule(0, true, false, now)

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

	newStage, nextReviewAt := nextSchedule(lastStage, true, false, now)

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
		stage, next = nextSchedule(stage, true, false, now)
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

	newStage, nextReviewAt := nextSchedule(3, false, false, now)

	if newStage != 0 {
		t.Errorf("newStage = %d, want 0 (a miss resets progress, not just one step back)", newStage)
	}
	if !nextReviewAt.Equal(now) {
		t.Errorf("nextReviewAt = %v, want %v (due immediately, retryable the same day rather than pushed to tomorrow)", nextReviewAt, now)
	}
}

func TestNextScheduleMissAtStageZeroStaysAtZero(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	newStage, nextReviewAt := nextSchedule(0, false, false, now)

	if newStage != 0 {
		t.Errorf("newStage = %d, want 0 (never goes negative)", newStage)
	}
	if !nextReviewAt.Equal(now) {
		t.Errorf("nextReviewAt = %v, want %v", nextReviewAt, now)
	}
}

// TestNextScheduleRepeatHoldsStageAndRepeatsTheSameInterval mirrors the
// learner-facing example driving the repeat flag: a word that took 30 days
// (stageIntervals[5]) to come up for review, marked "forced guess" via
// repeat, must come back in another 30 days — not advance to the 60-day
// step a confident correct answer would earn.
func TestNextScheduleRepeatHoldsStageAndRepeatsTheSameInterval(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	stage := 6 // reached via a prior correct answer from stage 5 (30-day interval)

	newStage, nextReviewAt := nextSchedule(stage, true, true, now)

	if newStage != stage {
		t.Errorf("newStage = %d, want %d (repeat holds the stage instead of advancing)", newStage, stage)
	}
	want := now.Add(stageIntervals[5]) // the 30-day interval that just elapsed, not the 60-day next step
	if !nextReviewAt.Equal(want) {
		t.Errorf("nextReviewAt = %v, want %v (repeat re-uses the interval that just elapsed)", nextReviewAt, want)
	}

	// A later confident (non-repeat) correct answer, still at the same
	// stage, resumes normal progress from exactly where repeat left it.
	nextStage, laterReviewAt := nextSchedule(newStage, true, false, now)
	if nextStage != stage+1 {
		t.Errorf("newStage = %d, want %d (a non-repeat correct answer resumes normal advancement)", nextStage, stage+1)
	}
	wantLater := now.Add(stageIntervals[5] * 2) // doubles past the table -> 60 days
	if !laterReviewAt.Equal(wantLater) {
		t.Errorf("nextReviewAt = %v, want %v", laterReviewAt, wantLater)
	}
}

// TestNextScheduleRepeatAtStageZeroStaysAtZero guards the prevStage
// underflow clamp: a repeat on a word's very first review (stage 0, never
// advanced past a miss) must not look up intervalForStage(-1).
func TestNextScheduleRepeatAtStageZeroStaysAtZero(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	newStage, nextReviewAt := nextSchedule(0, true, true, now)

	if newStage != 0 {
		t.Errorf("newStage = %d, want 0", newStage)
	}
	want := now.Add(stageIntervals[0])
	if !nextReviewAt.Equal(want) {
		t.Errorf("nextReviewAt = %v, want %v", nextReviewAt, want)
	}
}

// TestNextScheduleRepeatIsIgnoredWhenIncorrect makes sure repeat can't be
// (ab)used to soften a miss — an incorrect answer always resets to stage 0
// and reschedules due immediately, same as without the flag.
func TestNextScheduleRepeatIsIgnoredWhenIncorrect(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	newStage, nextReviewAt := nextSchedule(4, false, true, now)

	if newStage != 0 {
		t.Errorf("newStage = %d, want 0 (repeat must not soften a miss)", newStage)
	}
	if !nextReviewAt.Equal(now) {
		t.Errorf("nextReviewAt = %v, want %v", nextReviewAt, now)
	}
}
