package session

import (
	"reflect"
	"testing"

	"buddy/server/internal/llm"
)

func TestNewSnapshotHasOnlySystemPrompt(t *testing.T) {
	s := New("sys")
	got := s.Snapshot()
	want := []llm.Message{{Role: llm.RoleSystem, Content: "sys"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Snapshot() = %+v, want %+v", got, want)
	}
}

func TestAppendUserAssistantOrder(t *testing.T) {
	s := New("sys")
	s.AppendUser("hi")
	s.AppendAssistant("hello")
	got := s.Snapshot()
	want := []llm.Message{
		{Role: llm.RoleSystem, Content: "sys"},
		{Role: llm.RoleUser, Content: "hi"},
		{Role: llm.RoleAssistant, Content: "hello"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Snapshot() = %+v, want %+v", got, want)
	}
}

func TestSnapshotIncludesSummaryOnlyWhenSet(t *testing.T) {
	s := New("sys")
	if got := s.Snapshot(); len(got) != 1 {
		t.Fatalf("expected just the system message before Seed, got %+v", got)
	}

	s.Seed("learner likes hiking", nil, 0)
	got := s.Snapshot()
	if len(got) != 2 {
		t.Fatalf("expected system+summary messages, got %d: %+v", len(got), got)
	}
	if got[1].Role != llm.RoleSystem || got[1].Content == "" {
		t.Fatalf("summary message wrong: %+v", got[1])
	}
}

func TestSeedAndExportRoundTrip(t *testing.T) {
	s := New("sys")
	recent := []llm.Message{
		{Role: llm.RoleUser, Content: "hi"},
		{Role: llm.RoleAssistant, Content: "hello"},
	}
	s.Seed("summary text", recent, 0)

	gotSummary, gotRecent := s.Export()
	if gotSummary != "summary text" {
		t.Fatalf("summary = %q, want %q", gotSummary, "summary text")
	}
	if !reflect.DeepEqual(gotRecent, recent) {
		t.Fatalf("recent = %+v, want %+v", gotRecent, recent)
	}
}

func TestExportReturnsCopyNotAlias(t *testing.T) {
	s := New("sys")
	s.AppendUser("hi")

	_, recent := s.Export()
	recent[0].Content = "mutated"

	_, again := s.Export()
	if again[0].Content != "hi" {
		t.Fatalf("Export leaked its internal slice: got %q, want %q", again[0].Content, "hi")
	}
}

func TestReplaceLastUserUpdatesMostRecentUserOnly(t *testing.T) {
	s := New("sys")
	s.AppendUser("first draft")
	s.AppendAssistant("reply 1")
	s.AppendUser("second draft")
	s.ReplaceLastUser("second, corrected")

	_, recent := s.Export()
	if recent[0].Content != "first draft" {
		t.Fatalf("earlier user turn changed: %+v", recent)
	}
	if recent[2].Content != "second, corrected" {
		t.Fatalf("last user turn not replaced: %+v", recent)
	}
}

func TestReplaceLastUserNoUserTurnIsNoop(t *testing.T) {
	s := New("sys")
	s.ReplaceLastUser("nothing to replace") // must not panic
	_, recent := s.Export()
	if len(recent) != 0 {
		t.Fatalf("expected no history, got %+v", recent)
	}
}

func TestNextTurnIncrements(t *testing.T) {
	s := New("sys")
	if got := s.NextTurn(); got != 1 {
		t.Fatalf("first NextTurn() = %d, want 1", got)
	}
	if got := s.NextTurn(); got != 2 {
		t.Fatalf("second NextTurn() = %d, want 2", got)
	}
}

func TestNextTurnContinuesFromSeededLastTurn(t *testing.T) {
	s := New("sys")
	s.Seed("summary", nil, 5)
	if got := s.NextTurn(); got != 6 {
		t.Fatalf("NextTurn() after Seed(lastTurn=5) = %d, want 6", got)
	}
	if got := s.NextTurn(); got != 7 {
		t.Fatalf("NextTurn() = %d, want 7", got)
	}
}

func TestPeekOldestForCompactionBelowThreshold(t *testing.T) {
	s := New("sys")
	s.AppendUser("a")
	s.AppendAssistant("b")
	if _, _, ok := s.PeekOldestForCompaction(10); ok {
		t.Fatalf("expected ok=false when under threshold")
	}
	if _, _, ok := s.PeekOldestForCompaction(0); ok {
		t.Fatalf("expected ok=false for max<=0")
	}
}

func TestPeekOldestForCompactionDoesNotMutate(t *testing.T) {
	s := New("sys")
	for i := 0; i < 6; i++ {
		s.AppendUser("u")
		s.AppendAssistant("a")
	} // 12 messages total

	old, curSummary, ok := s.PeekOldestForCompaction(4)
	if !ok {
		t.Fatalf("expected ok=true when over threshold")
	}
	if curSummary != "" {
		t.Fatalf("curSummary = %q, want empty", curSummary)
	}
	wantDrop := 12 - 4/2 // fold down to half of max
	if len(old) != wantDrop {
		t.Fatalf("PeekOldestForCompaction returned %d messages, want %d", len(old), wantDrop)
	}

	_, recent := s.Export()
	if len(recent) != 12 {
		t.Fatalf("peek must not mutate history, len = %d, want 12", len(recent))
	}
}

func TestApplyCompactionDropsOldestAndSetsSummary(t *testing.T) {
	s := New("sys")
	for i := 0; i < 6; i++ {
		s.AppendUser("u")
		s.AppendAssistant("a")
	}
	old, _, ok := s.PeekOldestForCompaction(4)
	if !ok {
		t.Fatalf("expected ok=true")
	}
	s.ApplyCompaction("rolled-up summary", len(old))

	gotSummary, recent := s.Export()
	if gotSummary != "rolled-up summary" {
		t.Fatalf("summary = %q", gotSummary)
	}
	if len(recent) != 2 {
		t.Fatalf("expected 2 remaining verbatim messages, got %d: %+v", len(recent), recent)
	}
}

func TestApplyCompactionClampsNToHistoryLength(t *testing.T) {
	s := New("sys")
	s.AppendUser("only one")
	s.ApplyCompaction("summary", 999) // n larger than history
	_, recent := s.Export()
	if len(recent) != 0 {
		t.Fatalf("expected history fully drained, got %+v", recent)
	}
}
