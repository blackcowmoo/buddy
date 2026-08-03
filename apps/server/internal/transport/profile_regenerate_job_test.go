package transport

import (
	"context"
	"errors"
	"strings"
	"testing"

	"buddy/server/internal/llm"
	"buddy/server/internal/pipeline"
	"buddy/server/internal/protocol"
)

// recordingProfileLLM fakes the Analysis candidate UpdateLearnerProfile
// calls through: it extracts the "new session wrap-up" half of
// pipeline.renderLearnerProfileInput's rendered text (everything after its
// fixed marker line) so a test can assert both which session summaries were
// replayed and in what order, and returns a distinguishable, chainable
// "prevProfile > newSummary" result so a test can also confirm each call
// actually builds on the previous one's output rather than starting fresh
// every time.
type recordingProfileLLM struct {
	newSummaries *[]string
	failOnCall   int // 1-indexed; 0 means never fail
}

const newSummaryMarker = "New session wrap-up to fold in:\n"

func (r recordingProfileLLM) ChatStream(ctx context.Context, model string, msgs []llm.Message, onToken func(string)) (string, error) {
	return "", errFakeLLMUnavailable
}

func (r recordingProfileLLM) Complete(ctx context.Context, model string, msgs []llm.Message, jsonMode bool) (string, error) {
	var userMsg string
	for _, m := range msgs {
		if m.Role == llm.RoleUser {
			userMsg = m.Content
		}
	}
	idx := strings.Index(userMsg, newSummaryMarker)
	newSummary := userMsg[idx+len(newSummaryMarker):]
	*r.newSummaries = append(*r.newSummaries, newSummary)

	if r.failOnCall != 0 && len(*r.newSummaries) == r.failOnCall {
		return "", errors.New("llm unreachable")
	}

	prevMarker := "Previous profile:\n"
	prevEnd := strings.Index(userMsg, "\n\nNew session")
	prevSection := userMsg[len(prevMarker):prevEnd]
	if prevSection == "(none)" {
		return newSummary, nil
	}
	return prevSection + " > " + newSummary, nil
}

// endedSessionWithSummary is the realistic sequence a session actually goes
// through before it can appear in ListSessionsWithStudySummary: a saved
// turn (creates the row), EndSession, then CompleteStudySummary.
func endedSessionWithSummary(t *testing.T, st *fakeStore, sessionID, english string) {
	t.Helper()
	if err := st.SaveTurn(context.Background(), "alex", sessionID, 1, "user", "hi", false, protocol.SourceText); err != nil {
		t.Fatalf("SaveTurn(%s) error = %v", sessionID, err)
	}
	if err := st.EndSession(context.Background(), "alex", sessionID); err != nil {
		t.Fatalf("EndSession(%s) error = %v", sessionID, err)
	}
	if err := st.CompleteStudySummary(context.Background(), "alex", sessionID, []protocol.StudySummarySentence{
		{English: english, Translation: "번역"},
	}); err != nil {
		t.Fatalf("CompleteStudySummary(%s) error = %v", sessionID, err)
	}
}

// TestRunProfileRegenerateReplaysRemainingSessionsInEndOrder guards the core
// rebuild behavior: every remaining ended session with a study summary gets
// replayed through UpdateLearnerProfile oldest-ended-first (see
// store.MySQLStore.ListSessionsWithStudySummary), each fold building on the
// previous one's actual output — not starting fresh from the stale
// pre-deletion profile, which is the whole point of a from-scratch rebuild.
func TestRunProfileRegenerateReplaysRemainingSessionsInEndOrder(t *testing.T) {
	var newSummaries []string
	pipe := &pipeline.Pipeline{
		Analysis: []pipeline.Candidate{{Model: "m", LLM: recordingProfileLLM{newSummaries: &newSummaries}}},
	}
	st := newFakeStore()
	// This fake sorts ListSessionsWithStudySummary by session ID (see
	// ws_test.go's doc comment on it), so these IDs double as the intended
	// replay order.
	endedSessionWithSummary(t, st, "s-1", "S1")
	endedSessionWithSummary(t, st, "s-2", "S2")
	endedSessionWithSummary(t, st, "s-3", "S3")
	if err := st.SaveLearnerProfile(context.Background(), "alex", "stale profile from a now-deleted session"); err != nil {
		t.Fatalf("SaveLearnerProfile() error = %v", err)
	}

	if err := RunProfileRegenerateInline(context.Background(), pipe, st, "alex"); err != nil {
		t.Fatalf("RunProfileRegenerateInline() error = %v", err)
	}

	if want := []string{"S1", "S2", "S3"}; !stringSlicesEqual(newSummaries, want) {
		t.Fatalf("replayed session summaries = %v, want %v (in end order)", newSummaries, want)
	}
	got, _ := st.GetLearnerProfile(context.Background(), "alex")
	if want := "S1 > S2 > S3"; got != want {
		t.Fatalf("rebuilt profile = %q, want %q (each fold chained onto the previous one's real output)", got, want)
	}
}

// TestRunProfileRegenerateExcludesSessionsWithoutAStudySummary guards the
// filter itself from this package's side: a session that's ended but never
// flagged anything worth a wrap-up (empty study summary) must not be
// replayed — there's nothing to fold in, and the fake's own
// ListSessionsWithStudySummary already encodes this filter (mirroring the
// real store.MySQLStore query's WHERE clause).
func TestRunProfileRegenerateExcludesSessionsWithoutAStudySummary(t *testing.T) {
	var newSummaries []string
	pipe := &pipeline.Pipeline{
		Analysis: []pipeline.Candidate{{Model: "m", LLM: recordingProfileLLM{newSummaries: &newSummaries}}},
	}
	st := newFakeStore()
	endedSessionWithSummary(t, st, "s-1", "S1")
	// Ended, but never got a study summary (e.g. nothing was ever flagged) —
	// must be skipped.
	if err := st.SaveTurn(context.Background(), "alex", "s-2", 1, "user", "hi", false, protocol.SourceText); err != nil {
		t.Fatalf("SaveTurn(s-2) error = %v", err)
	}
	if err := st.EndSession(context.Background(), "alex", "s-2"); err != nil {
		t.Fatalf("EndSession(s-2) error = %v", err)
	}

	if err := RunProfileRegenerateInline(context.Background(), pipe, st, "alex"); err != nil {
		t.Fatalf("RunProfileRegenerateInline() error = %v", err)
	}

	if want := []string{"S1"}; !stringSlicesEqual(newSummaries, want) {
		t.Fatalf("replayed session summaries = %v, want %v", newSummaries, want)
	}
}

// TestRunProfileRegenerateSavesEmptyProfileWhenNothingIsLeft guards the
// "deleted everything that ever contributed" edge case: the rebuild must
// still run to completion and overwrite the stale profile with "", not
// leave the old (now entirely unearned) content in place.
func TestRunProfileRegenerateSavesEmptyProfileWhenNothingIsLeft(t *testing.T) {
	var newSummaries []string
	pipe := &pipeline.Pipeline{
		Analysis: []pipeline.Candidate{{Model: "m", LLM: recordingProfileLLM{newSummaries: &newSummaries}}},
	}
	st := newFakeStore()
	if err := st.SaveLearnerProfile(context.Background(), "alex", "stale profile from the one session that was just deleted"); err != nil {
		t.Fatalf("SaveLearnerProfile() error = %v", err)
	}

	if err := RunProfileRegenerateInline(context.Background(), pipe, st, "alex"); err != nil {
		t.Fatalf("RunProfileRegenerateInline() error = %v", err)
	}

	got, _ := st.GetLearnerProfile(context.Background(), "alex")
	if got != "" {
		t.Fatalf("rebuilt profile = %q, want empty (nothing left to rebuild from)", got)
	}
}

// TestRunProfileRegenerateAbortsWithoutSavingOnMidChainError guards the
// all-or-nothing contract (see runProfileRegenerate's doc comment): a
// failure partway through the replay chain must leave the existing profile
// completely untouched, not overwrite it with a partial rebuild — the
// reaper (for the queued path) retries the whole chain from scratch, and a
// half-replayed profile would be a worse state than a merely stale one.
func TestRunProfileRegenerateAbortsWithoutSavingOnMidChainError(t *testing.T) {
	var newSummaries []string
	pipe := &pipeline.Pipeline{
		Analysis: []pipeline.Candidate{{Model: "m", LLM: recordingProfileLLM{newSummaries: &newSummaries, failOnCall: 2}}},
	}
	st := newFakeStore()
	endedSessionWithSummary(t, st, "s-1", "S1")
	endedSessionWithSummary(t, st, "s-2", "S2")
	endedSessionWithSummary(t, st, "s-3", "S3")
	if err := st.SaveLearnerProfile(context.Background(), "alex", "untouched"); err != nil {
		t.Fatalf("SaveLearnerProfile() error = %v", err)
	}

	if err := RunProfileRegenerateInline(context.Background(), pipe, st, "alex"); err == nil {
		t.Fatal("RunProfileRegenerateInline() error = nil, want an error from the failed second call")
	}

	got, _ := st.GetLearnerProfile(context.Background(), "alex")
	if got != "untouched" {
		t.Fatalf("learner profile = %q, want it left completely untouched after a mid-chain failure", got)
	}
}

func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
