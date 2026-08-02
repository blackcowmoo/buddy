package store

import (
	"context"
	"testing"

	"buddy/server/internal/protocol"
)

// TestMySQLAssistantTurnTextReadsOnlyTheAssistantRow guards the reason
// AssistantTurnText scopes on role rather than just (session, turn): a user
// turn and its paired assistant turn share the same turn number, so without
// role-scoping the reply poller could hand the learner their own sentence
// back as the reply.
func TestMySQLAssistantTurnTextReadsOnlyTheAssistantRow(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()
	sessionID := "sess-assistant-text"
	if err := st.SaveTurn(ctx, "alex", sessionID, 1, "user", "how are you", false, protocol.SourceText); err != nil {
		t.Fatalf("SaveTurn(user) error = %v", err)
	}
	if err := st.SaveTurn(ctx, "alex", sessionID, 1, "assistant", "I'm good, thanks!", false, protocol.SourceText); err != nil {
		t.Fatalf("SaveTurn(assistant) error = %v", err)
	}
	text, err := st.AssistantTurnText(ctx, "alex", sessionID, 1)
	if err != nil {
		t.Fatalf("AssistantTurnText() error = %v", err)
	}
	if text != "I'm good, thanks!" {
		t.Fatalf("AssistantTurnText() = %q, want the assistant row's text", text)
	}
}

// TestMySQLAssistantTurnTextEmptyWhenNoAssistantRow covers what the reply
// poller sees for a turn whose assistant row doesn't exist (or is still the
// placeholder ReserveAssistantTurn wrote): "" and no error, so the poll path
// distinguishes "nothing to deliver" from a real query failure.
func TestMySQLAssistantTurnTextEmptyWhenNoAssistantRow(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()
	text, err := st.AssistantTurnText(ctx, "alex", "sess-no-assistant-row", 1)
	if err != nil {
		t.Fatalf("AssistantTurnText() error = %v, want nil for a missing row", err)
	}
	if text != "" {
		t.Fatalf("AssistantTurnText() = %q, want \"\"", text)
	}
}

func TestMySQLReserveAssistantTurnCreatesPendingPlaceholder(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()
	sessionID := "sess-reserve-placeholder"
	// A real reply job always follows the learner's own turn, which is what
	// creates the session row (see SaveTurn) — turn 1's reply job needs that
	// row to already exist for SessionDetail to find it.
	if err := st.SaveTurn(ctx, "alex", sessionID, 1, "user", "hello", false, protocol.SourceText); err != nil {
		t.Fatalf("SaveTurn(user) error = %v", err)
	}
	if err := st.ReserveAssistantTurn(ctx, "alex", sessionID, 1); err != nil {
		t.Fatalf("ReserveAssistantTurn() error = %v", err)
	}

	status, err := st.JobStatus(ctx, "alex", sessionID, 1, "reply")
	if err != nil {
		t.Fatalf("JobStatus() error = %v", err)
	}
	if status != JobStatusPending {
		t.Fatalf("JobStatus() = %q, want %q", status, JobStatusPending)
	}

	_, turns, err := st.SessionDetail(ctx, "alex", sessionID)
	if err != nil {
		t.Fatalf("SessionDetail() error = %v", err)
	}
	var assistant *Turn
	for i := range turns {
		if turns[i].Turn == 1 && turns[i].Role == "assistant" {
			assistant = &turns[i]
		}
	}
	if assistant == nil {
		t.Fatalf("no placeholder assistant turn found: %+v", turns)
	}
	if assistant.Text != "" {
		t.Fatalf("placeholder Text = %q, want empty", assistant.Text)
	}
	if assistant.ReplyStatus != JobStatusPending {
		t.Fatalf("placeholder ReplyStatus = %q, want %q", assistant.ReplyStatus, JobStatusPending)
	}
}

// TestMySQLReserveAssistantTurnIsIdempotent guards the race this is built to
// survive: a second reservation for a turn that's already been completed
// (e.g. a stale retry racing an already-finished job) must not resurrect an
// empty placeholder over the real text.
func TestMySQLReserveAssistantTurnIsIdempotent(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()
	sessionID := "sess-reserve-idempotent"
	if err := st.SaveTurn(ctx, "alex", sessionID, 1, "user", "hello", false, protocol.SourceText); err != nil {
		t.Fatalf("SaveTurn(user) error = %v", err)
	}
	if err := st.ReserveAssistantTurn(ctx, "alex", sessionID, 1); err != nil {
		t.Fatalf("ReserveAssistantTurn(1) error = %v", err)
	}
	if err := st.CompleteAssistantTurn(ctx, "alex", sessionID, 1, "Nice to meet you!"); err != nil {
		t.Fatalf("CompleteAssistantTurn() error = %v", err)
	}
	// A second reservation arriving after completion (e.g. a reaped retry
	// that lost the race) must be a no-op.
	if err := st.ReserveAssistantTurn(ctx, "alex", sessionID, 1); err != nil {
		t.Fatalf("ReserveAssistantTurn(2) error = %v", err)
	}

	status, err := st.JobStatus(ctx, "alex", sessionID, 1, "reply")
	if err != nil {
		t.Fatalf("JobStatus() error = %v", err)
	}
	if status != JobStatusDone {
		t.Fatalf("JobStatus() = %q, want %q (second reserve must not revert it)", status, JobStatusDone)
	}
	_, turns, err := st.SessionDetail(ctx, "alex", sessionID)
	if err != nil {
		t.Fatalf("SessionDetail() error = %v", err)
	}
	for _, tn := range turns {
		if tn.Turn == 1 && tn.Role == "assistant" && tn.Text != "Nice to meet you!" {
			t.Fatalf("assistant Text = %q, want the completed text preserved", tn.Text)
		}
	}
}

func TestMySQLCompleteAssistantTurnSetsTextAndDoneStatus(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()
	sessionID := "sess-complete"
	if err := st.SaveTurn(ctx, "alex", sessionID, 1, "user", "hello", false, protocol.SourceText); err != nil {
		t.Fatalf("SaveTurn(user) error = %v", err)
	}
	if err := st.ReserveAssistantTurn(ctx, "alex", sessionID, 1); err != nil {
		t.Fatalf("ReserveAssistantTurn() error = %v", err)
	}
	if err := st.CompleteAssistantTurn(ctx, "alex", sessionID, 1, "Hi there!"); err != nil {
		t.Fatalf("CompleteAssistantTurn() error = %v", err)
	}

	status, err := st.JobStatus(ctx, "alex", sessionID, 1, "reply")
	if err != nil {
		t.Fatalf("JobStatus() error = %v", err)
	}
	if status != JobStatusDone {
		t.Fatalf("JobStatus() = %q, want %q", status, JobStatusDone)
	}
	_, turns, err := st.SessionDetail(ctx, "alex", sessionID)
	if err != nil {
		t.Fatalf("SessionDetail() error = %v", err)
	}
	var found bool
	for _, tn := range turns {
		if tn.Turn == 1 && tn.Role == "assistant" {
			found = true
			if tn.Text != "Hi there!" {
				t.Fatalf("Text = %q, want %q", tn.Text, "Hi there!")
			}
			if tn.ReplyStatus != JobStatusDone {
				t.Fatalf("ReplyStatus = %q, want %q", tn.ReplyStatus, JobStatusDone)
			}
		}
	}
	if !found {
		t.Fatalf("no assistant turn found: %+v", turns)
	}
}

// TestMySQLCompleteAssistantTurnAtTurnZeroCreatesVisibleSession guards that
// moving the opening greeting onto Reserve/CompleteAssistantTurn preserves
// SaveTurn's turn-0 behavior: a greeting-only room must still show up in
// ListSessions/SessionDetail even if the learner never replies (see
// TestMySQLSaveTurnGreetingAloneCreatesVisibleSession for the SaveTurn-path
// equivalent this mirrors).
func TestMySQLCompleteAssistantTurnAtTurnZeroCreatesVisibleSession(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()
	userID := "greeting-job-user"
	sessionID := "sess-greeting-job-only"
	if err := st.ReserveAssistantTurn(ctx, userID, sessionID, 0); err != nil {
		t.Fatalf("ReserveAssistantTurn() error = %v", err)
	}
	if err := st.CompleteAssistantTurn(ctx, userID, sessionID, 0, "Hi! How was your day?"); err != nil {
		t.Fatalf("CompleteAssistantTurn() error = %v", err)
	}

	meta, turns, err := st.SessionDetail(ctx, userID, sessionID)
	if err != nil {
		t.Fatalf("SessionDetail() error = %v", err)
	}
	if meta.Title != "Hi! How was your day?" {
		t.Fatalf("Title = %q, want the greeting text", meta.Title)
	}
	if len(turns) != 1 || turns[0].Text != "Hi! How was your day?" {
		t.Fatalf("turns = %+v, want just the greeting", turns)
	}

	sessions, err := st.ListSessions(ctx, userID)
	if err != nil {
		t.Fatalf("ListSessions() error = %v", err)
	}
	if len(sessions) != 1 || sessions[0].ID != sessionID {
		t.Fatalf("ListSessions() = %+v, want the greeting-only room listed", sessions)
	}
}

func TestMySQLFailJobSetsFailedStatus(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()
	sessionID := "sess-fail-job"
	if err := st.SaveTurn(ctx, "alex", sessionID, 1, "user", "hello", false, protocol.SourceText); err != nil {
		t.Fatalf("SaveTurn(user) error = %v", err)
	}
	if err := st.ReserveAssistantTurn(ctx, "alex", sessionID, 1); err != nil {
		t.Fatalf("ReserveAssistantTurn() error = %v", err)
	}
	if err := st.FailJob(ctx, "alex", sessionID, 1, "reply", "llm: unavailable"); err != nil {
		t.Fatalf("FailJob() error = %v", err)
	}

	status, err := st.JobStatus(ctx, "alex", sessionID, 1, "reply")
	if err != nil {
		t.Fatalf("JobStatus() error = %v", err)
	}
	if status != JobStatusFailed {
		t.Fatalf("JobStatus() = %q, want %q", status, JobStatusFailed)
	}
}

func TestMySQLJobStatusEmptyWhenNeverReserved(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()
	sessionID := "sess-job-status-empty"
	if err := st.SaveTurn(ctx, "alex", sessionID, 1, "user", "hello", false, protocol.SourceText); err != nil {
		t.Fatalf("SaveTurn(user) error = %v", err)
	}
	// The assistant turn here is saved the old way (plain SaveTurn), as every
	// turn was before this feature existed — it must not appear to have any
	// reply-job status.
	if err := st.SaveTurn(ctx, "alex", sessionID, 1, "assistant", "hi", false, ""); err != nil {
		t.Fatalf("SaveTurn(assistant) error = %v", err)
	}

	status, err := st.JobStatus(ctx, "alex", sessionID, 1, "reply")
	if err != nil {
		t.Fatalf("JobStatus() error = %v", err)
	}
	if status != "" {
		t.Fatalf("JobStatus() = %q, want empty (never reserved)", status)
	}

	_, turns, err := st.SessionDetail(ctx, "alex", sessionID)
	if err != nil {
		t.Fatalf("SessionDetail() error = %v", err)
	}
	for _, tn := range turns {
		if tn.ReplyStatus != "" {
			t.Fatalf("turn %+v has ReplyStatus %q, want empty for a turn never tracked as a job", tn, tn.ReplyStatus)
		}
	}
}

// TestMySQLFailJobSetsCorrectionFailedStatus is the case the frontend
// actually needed: a correction job that errored out must be distinguishable
// from "the sentence needed no fix" (Correction present, empty Issues) —
// both look identical in buddy_turns.correction alone, so the frontend relies
// on CorrectionStatus == JobStatusFailed instead (see store.Turn.CorrectionStatus).
func TestMySQLFailJobSetsCorrectionFailedStatus(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()
	sessionID := "sess-fail-correction"
	if err := st.SaveTurn(ctx, "alex", sessionID, 1, "user", "I go to school yesterday", false, protocol.SourceText); err != nil {
		t.Fatalf("SaveTurn(user) error = %v", err)
	}
	if err := st.ReserveCorrectionJob(ctx, "alex", sessionID, 1); err != nil {
		t.Fatalf("ReserveCorrectionJob() error = %v", err)
	}
	if err := st.FailJob(ctx, "alex", sessionID, 1, "correction", "llm: bad json"); err != nil {
		t.Fatalf("FailJob() error = %v", err)
	}

	status, err := st.JobStatus(ctx, "alex", sessionID, 1, "correction")
	if err != nil {
		t.Fatalf("JobStatus() error = %v", err)
	}
	if status != JobStatusFailed {
		t.Fatalf("JobStatus() = %q, want %q", status, JobStatusFailed)
	}

	_, turns, err := st.SessionDetail(ctx, "alex", sessionID)
	if err != nil {
		t.Fatalf("SessionDetail() error = %v", err)
	}
	for _, tn := range turns {
		if tn.Turn == 1 && tn.Role == "user" {
			if tn.CorrectionStatus != JobStatusFailed {
				t.Fatalf("CorrectionStatus = %q, want %q", tn.CorrectionStatus, JobStatusFailed)
			}
			if tn.Correction != nil {
				t.Fatalf("Correction = %+v, want nil on a failed job", tn.Correction)
			}
		}
	}
}
