package store

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"buddy/server/internal/protocol"
)

func TestMySQLSaveTurnCreatesSessionWithTitleFromFirstMessage(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()
	sessionID := "sess-title"
	if err := st.SaveTurn(ctx, "alex", sessionID, 1, "user", "My name is Alex and I like hiking.", false, protocol.SourceText); err != nil {
		t.Fatalf("SaveTurn() error = %v", err)
	}
	meta, _, err := st.SessionDetail(ctx, "alex", sessionID)
	if err != nil {
		t.Fatalf("SessionDetail() error = %v", err)
	}
	if meta.Title != "My name is Alex and I like hiking." {
		t.Fatalf("Title = %q, want the first message verbatim", meta.Title)
	}
}

// TestMySQLLastTurnReturnsZeroForUnknownSession and
// TestMySQLLastTurnReturnsHighestPersistedTurn guard the fix for reopening a
// chat room after a while wiping out earlier messages: session.Session.Seed
// uses LastTurn to resume turn numbering instead of restarting at 0, which is
// what let a reconnect's own turn 1 silently overwrite the original turn 1
// via SaveTurn's ON DUPLICATE KEY UPDATE.
func TestMySQLLastTurnReturnsZeroForUnknownSession(t *testing.T) {
	st := requireStore(t)
	last, err := st.LastTurn(context.Background(), "alex", "no-such-session")
	if err != nil {
		t.Fatalf("LastTurn() error = %v", err)
	}
	if last != 0 {
		t.Fatalf("LastTurn() = %d, want 0 for an unknown session", last)
	}
}

func TestMySQLLastTurnReturnsHighestPersistedTurn(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()
	sessionID := "sess-last-turn"
	if err := st.SaveTurn(ctx, "alex", sessionID, 1, "user", "first", false, protocol.SourceText); err != nil {
		t.Fatalf("SaveTurn(1) error = %v", err)
	}
	if err := st.SaveTurn(ctx, "alex", sessionID, 1, "assistant", "reply", false, ""); err != nil {
		t.Fatalf("SaveTurn(1, assistant) error = %v", err)
	}
	if err := st.SaveTurn(ctx, "alex", sessionID, 2, "user", "second", false, protocol.SourceText); err != nil {
		t.Fatalf("SaveTurn(2) error = %v", err)
	}
	last, err := st.LastTurn(ctx, "alex", sessionID)
	if err != nil {
		t.Fatalf("LastTurn() error = %v", err)
	}
	if last != 2 {
		t.Fatalf("LastTurn() = %d, want 2", last)
	}
}

// TestMySQLLastTurnScopedPerUserAndSession guards LastTurn's WHERE clause:
// without both user_id and session_id in scope, one user's turn numbers
// could leak into another's LastTurn result and desync their session's turn
// counter on reconnect (see internal/transport, which seeds from this).
func TestMySQLLastTurnScopedPerUserAndSession(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()
	if err := st.SaveTurn(ctx, "alex", "sess-a", 5, "user", "msg", false, protocol.SourceText); err != nil {
		t.Fatalf("SaveTurn() error = %v", err)
	}
	if err := st.SaveTurn(ctx, "sam", "sess-b", 9, "user", "msg", false, protocol.SourceText); err != nil {
		t.Fatalf("SaveTurn() error = %v", err)
	}
	last, err := st.LastTurn(ctx, "alex", "sess-a")
	if err != nil {
		t.Fatalf("LastTurn() error = %v", err)
	}
	if last != 5 {
		t.Fatalf("LastTurn(alex/sess-a) = %d, want 5, unaffected by sam/sess-b", last)
	}
}

func TestMySQLSaveTurnTitleSurvivesLaterTurns(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()
	sessionID := "sess-title-stable"
	if err := st.SaveTurn(ctx, "alex", sessionID, 1, "user", "first message", false, protocol.SourceText); err != nil {
		t.Fatalf("SaveTurn(1) error = %v", err)
	}
	if err := st.SaveTurn(ctx, "alex", sessionID, 1, "assistant", "reply", false, ""); err != nil {
		t.Fatalf("SaveTurn(assistant) error = %v", err)
	}
	if err := st.SaveTurn(ctx, "alex", sessionID, 2, "user", "second message", false, protocol.SourceText); err != nil {
		t.Fatalf("SaveTurn(2) error = %v", err)
	}
	meta, _, err := st.SessionDetail(ctx, "alex", sessionID)
	if err != nil {
		t.Fatalf("SessionDetail() error = %v", err)
	}
	if meta.Title != "first message" {
		t.Fatalf("Title = %q, want it to stay pinned to the opening message", meta.Title)
	}
}

// TestMySQLSaveTurnGreetingAloneCreatesVisibleSession guards the fix for a
// room the learner opened (got the opening greeting) but never replied to:
// it must show up in ListSessions/SessionDetail — titled from the greeting
// itself — instead of leaving an invisible orphan in buddy_turns with no row
// in buddy_sessions to find it by.
func TestMySQLSaveTurnGreetingAloneCreatesVisibleSession(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()
	// A dedicated user ID (not "alex", reused by many other tests sharing
	// this store) so the ListSessions assertion below only sees this test's
	// own room.
	userID := "greeting-only-user"
	sessionID := "sess-greeting-only"
	if err := st.SaveTurn(ctx, userID, sessionID, 0, "assistant", "Hi! How was your day?", false, ""); err != nil {
		t.Fatalf("SaveTurn(greeting) error = %v", err)
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

// TestMySQLSaveTurnUserReplyReplacesGreetingTitle guards that once the
// learner does reply, their own first message becomes the title instead of
// staying pinned to the greeting placeholder SaveTurn set at turn 0.
func TestMySQLSaveTurnUserReplyReplacesGreetingTitle(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()
	sessionID := "sess-greeting-then-reply"
	if err := st.SaveTurn(ctx, "alex", sessionID, 0, "assistant", "Hi! How was your day?", false, ""); err != nil {
		t.Fatalf("SaveTurn(greeting) error = %v", err)
	}
	if err := st.SaveTurn(ctx, "alex", sessionID, 1, "user", "It was great, thanks!", false, protocol.SourceText); err != nil {
		t.Fatalf("SaveTurn(reply) error = %v", err)
	}
	meta, _, err := st.SessionDetail(ctx, "alex", sessionID)
	if err != nil {
		t.Fatalf("SessionDetail() error = %v", err)
	}
	if meta.Title != "It was great, thanks!" {
		t.Fatalf("Title = %q, want the learner's own first message", meta.Title)
	}

	var n int
	if err := st.rw.QueryRowContext(ctx, `SELECT count(*) FROM `+sessionsTable+` WHERE user_id = ? AND id = ?`, "alex", sessionID).Scan(&n); err != nil {
		t.Fatalf("count query: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected exactly 1 row, got %d", n)
	}
}

func TestMySQLSaveTurnUpsertsRefinedText(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()
	sessionID := "sess-refine"
	if err := st.SaveTurn(ctx, "alex", sessionID, 1, "user", "fast stt guess", false, protocol.SourceVoice); err != nil {
		t.Fatalf("SaveTurn(fast) error = %v", err)
	}
	if err := st.SaveTurn(ctx, "alex", sessionID, 1, "user", "refined transcript", true, protocol.SourceVoice); err != nil {
		t.Fatalf("SaveTurn(refined) error = %v", err)
	}
	_, turns, err := st.SessionDetail(ctx, "alex", sessionID)
	if err != nil {
		t.Fatalf("SessionDetail() error = %v", err)
	}
	if len(turns) != 1 {
		t.Fatalf("expected the refine to upsert onto the same row, got %d turns: %+v", len(turns), turns)
	}
	if turns[0].Text != "refined transcript" || !turns[0].Refined {
		t.Fatalf("turn = %+v, want refined text with Refined=true", turns[0])
	}
}

// TestMySQLSaveTurnPersistsSource guards the source column added to
// distinguish spoken input from typed input: a user turn's source must
// round-trip through SessionDetail, and an assistant turn (which has none)
// must come back empty rather than inheriting whatever the user turn had.
func TestMySQLSaveTurnPersistsSource(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()
	sessionID := "sess-source"
	if err := st.SaveTurn(ctx, "alex", sessionID, 1, "user", "spoken sentence", false, protocol.SourceVoice); err != nil {
		t.Fatalf("SaveTurn(user) error = %v", err)
	}
	if err := st.SaveTurn(ctx, "alex", sessionID, 1, "assistant", "reply", false, ""); err != nil {
		t.Fatalf("SaveTurn(assistant) error = %v", err)
	}
	_, turns, err := st.SessionDetail(ctx, "alex", sessionID)
	if err != nil {
		t.Fatalf("SessionDetail() error = %v", err)
	}
	if len(turns) != 2 || turns[0].Role != "user" || turns[1].Role != "assistant" {
		t.Fatalf("turns = %+v, want [user, assistant]", turns)
	}
	if turns[0].Source != protocol.SourceVoice {
		t.Fatalf("user turn Source = %q, want %q", turns[0].Source, protocol.SourceVoice)
	}
	if turns[1].Source != "" {
		t.Fatalf("assistant turn Source = %q, want empty", turns[1].Source)
	}
}

// TestMySQLSaveTurnUpsertsSource guards the ON DUPLICATE KEY UPDATE clause:
// re-saving a turn under a different source (e.g. a refine pass, which is
// always voice) must overwrite the row's source, not just its text/refined.
func TestMySQLSaveTurnUpsertsSource(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()
	sessionID := "sess-source-upsert"
	if err := st.SaveTurn(ctx, "alex", sessionID, 1, "user", "typed then corrected by voice", false, protocol.SourceText); err != nil {
		t.Fatalf("SaveTurn(text) error = %v", err)
	}
	if err := st.SaveTurn(ctx, "alex", sessionID, 1, "user", "typed then corrected by voice", false, protocol.SourceVoice); err != nil {
		t.Fatalf("SaveTurn(voice) error = %v", err)
	}
	_, turns, err := st.SessionDetail(ctx, "alex", sessionID)
	if err != nil {
		t.Fatalf("SessionDetail() error = %v", err)
	}
	if len(turns) != 1 || turns[0].Source != protocol.SourceVoice {
		t.Fatalf("turns = %+v, want a single turn with Source=%q", turns, protocol.SourceVoice)
	}
}

func TestMySQLSaveCorrectionAttachesToExistingTurn(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()
	sessionID := "sess-correction"
	if err := st.SaveTurn(ctx, "alex", sessionID, 1, "user", "he go school", false, protocol.SourceText); err != nil {
		t.Fatalf("SaveTurn() error = %v", err)
	}
	c := protocol.Correction{
		Original:  "he go school",
		Corrected: "he goes to school",
		Issues: []protocol.Issue{
			{Type: "grammar", Span: "go", Suggestion: "goes", Explanation: "3인칭 단수"},
		},
	}
	if err := st.SaveCorrection(ctx, "alex", sessionID, 1, c); err != nil {
		t.Fatalf("SaveCorrection() error = %v", err)
	}
	_, turns, err := st.SessionDetail(ctx, "alex", sessionID)
	if err != nil {
		t.Fatalf("SessionDetail() error = %v", err)
	}
	if len(turns) != 1 || turns[0].Correction == nil {
		t.Fatalf("expected correction attached to turn 1, got %+v", turns)
	}
	if !reflect.DeepEqual(*turns[0].Correction, c) {
		t.Fatalf("Correction = %+v, want %+v", *turns[0].Correction, c)
	}
}

// TestMySQLSaveCorrectionNoopWhenTurnMissing covers the ordering guarantee
// SaveCorrection relies on instead of an upsert: it only ever UPDATEs an
// existing row, so a correction that (in theory) raced ahead of its turn
// must not fabricate one.
func TestMySQLSaveCorrectionNoopWhenTurnMissing(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()
	err := st.SaveCorrection(ctx, "alex", "sess-missing-turn", 1, protocol.Correction{Original: "x", Corrected: "x"})
	if err != nil {
		t.Fatalf("SaveCorrection() error = %v, want nil (no-op)", err)
	}
	_, _, err = st.SessionDetail(ctx, "alex", "sess-missing-turn")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("SessionDetail() error = %v, want ErrNotFound (no session should have been created)", err)
	}
}

// The Chat preview is readable but non-terminal; only a changed Judge result
// becomes unread, and opening it must clear the server-owned marker without a
// late preview or idempotent job retry bringing it back.
func TestMySQLCorrectionStagesAndUnreadAcknowledgementRoundTrip(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()
	const userID = "correction-stage-user"
	const sessionID = "sess-correction-stages"
	preview := protocol.Correction{Original: "I are fine.", Corrected: "I am fine.", Issues: []protocol.Issue{}, Translation: "나는 괜찮아."}
	changedFinal := protocol.Correction{
		Original:    "I are fine.",
		Corrected:   "I am fine.",
		Issues:      []protocol.Issue{},
		Translation: "나는 잘 지내.",
	}

	if err := st.SaveTurn(ctx, userID, sessionID, 1, "user", preview.Original, false, protocol.SourceText); err != nil {
		t.Fatalf("SaveTurn(1) error = %v", err)
	}
	if err := st.ReserveCorrectionJob(ctx, userID, sessionID, 1); err != nil {
		t.Fatalf("ReserveCorrectionJob(1) error = %v", err)
	}
	if err := st.SaveCorrectionPreview(ctx, userID, sessionID, 1, preview); err != nil {
		t.Fatalf("SaveCorrectionPreview(1) error = %v", err)
	}
	meta, turns, err := st.SessionDetail(ctx, userID, sessionID)
	if err != nil {
		t.Fatalf("SessionDetail(preview) error = %v", err)
	}
	if meta.UnreadCorrections != 0 || len(turns) != 1 || turns[0].CorrectionStage != "chat" || turns[0].CorrectionUnread || turns[0].CorrectionStatus != JobStatusPending {
		t.Fatalf("preview state = meta %+v turns %+v, want readable chat stage, pending and read", meta, turns)
	}

	if err := st.SaveCorrectionFinal(ctx, userID, sessionID, 1, preview, false); err != nil {
		t.Fatalf("SaveCorrectionFinal(same final) error = %v", err)
	}
	meta, turns, err = st.SessionDetail(ctx, userID, sessionID)
	if err != nil {
		t.Fatalf("SessionDetail(same final) error = %v", err)
	}
	if meta.UnreadCorrections != 0 || turns[0].CorrectionStage != "judge" || turns[0].CorrectionUnread || turns[0].CorrectionStatus != JobStatusDone {
		t.Fatalf("same final state = meta %+v turn %+v, want terminal judge stage without unread", meta, turns[0])
	}
	if err := st.SaveCorrectionPreview(ctx, userID, sessionID, 1, changedFinal); err != nil {
		t.Fatalf("late SaveCorrectionPreview error = %v", err)
	}
	_, turns, err = st.SessionDetail(ctx, userID, sessionID)
	if err != nil || !reflect.DeepEqual(*turns[0].Correction, preview) {
		t.Fatalf("late preview overwrote Judge final: turns=%+v err=%v", turns, err)
	}

	if err := st.SaveTurn(ctx, userID, sessionID, 2, "user", preview.Original, false, protocol.SourceText); err != nil {
		t.Fatalf("SaveTurn(2) error = %v", err)
	}
	if err := st.ReserveCorrectionJob(ctx, userID, sessionID, 2); err != nil {
		t.Fatalf("ReserveCorrectionJob(2) error = %v", err)
	}
	if err := st.SaveCorrectionPreview(ctx, userID, sessionID, 2, preview); err != nil {
		t.Fatalf("SaveCorrectionPreview(2) error = %v", err)
	}
	if err := st.SaveCorrectionFinal(ctx, userID, sessionID, 2, changedFinal, true); err != nil {
		t.Fatalf("SaveCorrectionFinal(changed final) error = %v", err)
	}
	meta, turns, err = st.SessionDetail(ctx, userID, sessionID)
	if err != nil {
		t.Fatalf("SessionDetail(changed final) error = %v", err)
	}
	if meta.UnreadCorrections != 1 || len(turns) != 2 || !turns[1].CorrectionUnread || turns[1].CorrectionStage != "judge" {
		t.Fatalf("changed final state = meta %+v turns %+v, want exactly one unread Judge result", meta, turns)
	}
	sessions, err := st.ListSessions(ctx, userID)
	if err != nil || len(sessions) != 1 || sessions[0].UnreadCorrections != 1 {
		t.Fatalf("ListSessions unread count = %+v, err=%v; want 1", sessions, err)
	}

	if err := st.MarkCorrectionRead(ctx, "someone-else", sessionID, 2); err != nil {
		t.Fatalf("MarkCorrectionRead(other user) error = %v", err)
	}
	if err := st.MarkCorrectionRead(ctx, userID, sessionID, 2); err != nil {
		t.Fatalf("MarkCorrectionRead(owner) error = %v", err)
	}
	// A duplicate completion can arrive after acknowledgement when the queue
	// retries. It must preserve the already-read state for the same Judge JSON.
	if err := st.SaveCorrectionFinal(ctx, userID, sessionID, 2, changedFinal, true); err != nil {
		t.Fatalf("SaveCorrectionFinal(idempotent retry) error = %v", err)
	}
	meta, turns, err = st.SessionDetail(ctx, userID, sessionID)
	if err != nil || meta.UnreadCorrections != 0 || turns[1].CorrectionUnread {
		t.Fatalf("acknowledged state returned after retry: meta=%+v turns=%+v err=%v", meta, turns, err)
	}

	// persistEvent intentionally detaches preview/final writes. Prove the DB
	// result is still correct when their completion order is inverted.
	if err := st.SaveTurn(ctx, userID, sessionID, 3, "user", preview.Original, false, protocol.SourceText); err != nil {
		t.Fatalf("SaveTurn(3) error = %v", err)
	}
	if err := st.ReserveCorrectionJob(ctx, userID, sessionID, 3); err != nil {
		t.Fatalf("ReserveCorrectionJob(3) error = %v", err)
	}
	if err := st.SaveCorrectionFinal(ctx, userID, sessionID, 3, preview, false); err != nil {
		t.Fatalf("SaveCorrectionFinal(before preview) error = %v", err)
	}
	if err := st.SaveCorrectionPreview(ctx, userID, sessionID, 3, changedFinal); err != nil {
		t.Fatalf("SaveCorrectionPreview(late) error = %v", err)
	}
	meta, turns, err = st.SessionDetail(ctx, userID, sessionID)
	if err != nil || len(turns) != 3 || turns[2].CorrectionStage != "judge" || turns[2].CorrectionUnread || !reflect.DeepEqual(*turns[2].Correction, preview) {
		t.Fatalf("inverted persistence order = meta=%+v turns=%+v err=%v", meta, turns, err)
	}
}

// TestMySQLSaveTranslationAttachesToCorrectRole guards the reason
// SaveTranslation takes role in its WHERE clause instead of just
// (session, turn): a user turn and its paired assistant turn share the same
// turn number, so without role-scoping a translation could land on the
// wrong row.
func TestMySQLSaveTranslationAttachesToCorrectRole(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()
	sessionID := "sess-translation"
	if err := st.SaveTurn(ctx, "alex", sessionID, 1, "user", "he go school", false, protocol.SourceText); err != nil {
		t.Fatalf("SaveTurn(user) error = %v", err)
	}
	if err := st.SaveTurn(ctx, "alex", sessionID, 1, "assistant", "Nice!", false, ""); err != nil {
		t.Fatalf("SaveTurn(assistant) error = %v", err)
	}
	if err := st.SaveTranslation(ctx, "alex", sessionID, 1, "user", "그는 학교에 간다"); err != nil {
		t.Fatalf("SaveTranslation(user) error = %v", err)
	}
	if err := st.SaveTranslation(ctx, "alex", sessionID, 1, "assistant", "좋아요!"); err != nil {
		t.Fatalf("SaveTranslation(assistant) error = %v", err)
	}
	_, turns, err := st.SessionDetail(ctx, "alex", sessionID)
	if err != nil {
		t.Fatalf("SessionDetail() error = %v", err)
	}
	if len(turns) != 2 || turns[0].Role != "user" || turns[1].Role != "assistant" {
		t.Fatalf("turns = %+v, want [user, assistant]", turns)
	}
	if turns[0].Translation != "그는 학교에 간다" {
		t.Fatalf("user translation = %q", turns[0].Translation)
	}
	if turns[1].Translation != "좋아요!" {
		t.Fatalf("assistant translation = %q", turns[1].Translation)
	}
}

// TestMySQLSaveTranslationNoopWhenTurnMissing mirrors
// TestMySQLSaveCorrectionNoopWhenTurnMissing: SaveTranslation only ever
// UPDATEs an existing row, never fabricates one.
func TestMySQLSaveTranslationNoopWhenTurnMissing(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()
	err := st.SaveTranslation(ctx, "alex", "sess-missing-turn-translation", 1, "user", "번역")
	if err != nil {
		t.Fatalf("SaveTranslation() error = %v, want nil (no-op)", err)
	}
	_, _, err = st.SessionDetail(ctx, "alex", "sess-missing-turn-translation")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("SessionDetail() error = %v, want ErrNotFound (no session should have been created)", err)
	}
}

// TestMySQLSaveTurnLaterTurnsBumpUpdatedAt guards the fix for a room list
// that could sort stale until the connection's 30s save ticker (or its
// on-disconnect save) happened to fire: SaveTurn only touched updated_at on
// turn 0 or the first user turn, so a room that had already exchanged a few
// turns wouldn't sort as the most recent until well after the learner's last
// message actually landed.
func TestMySQLSaveTurnLaterTurnsBumpUpdatedAt(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()
	userID := "later-turn-order-user"
	if err := st.SaveTurn(ctx, userID, "s-old", 1, "user", "older room", false, protocol.SourceText); err != nil {
		t.Fatalf("SaveTurn(old, 1) error = %v", err)
	}
	if err := st.SaveTurn(ctx, userID, "s-new", 1, "user", "newer room, first turn", false, protocol.SourceText); err != nil {
		t.Fatalf("SaveTurn(new, 1) error = %v", err)
	}
	time.Sleep(1100 * time.Millisecond) // updated_at has 1-second resolution (UNIX_TIMESTAMP())

	// A later turn (turn 2, well past the ensureSessionRow guard) on the
	// *older* room should still bump it back to the top.
	if err := st.SaveTurn(ctx, userID, "s-old", 2, "assistant", "a later reply", false, ""); err != nil {
		t.Fatalf("SaveTurn(old, 2) error = %v", err)
	}

	sessions, err := st.ListSessions(ctx, userID)
	if err != nil {
		t.Fatalf("ListSessions() error = %v", err)
	}
	if len(sessions) != 2 || sessions[0].ID != "s-old" || sessions[1].ID != "s-new" {
		t.Fatalf("ListSessions() = %+v, want [s-old, s-new] after s-old's later turn", sessions)
	}
}

func TestMySQLReserveCorrectionJobCreatesPendingStatus(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()
	sessionID := "sess-reserve-correction"
	if err := st.SaveTurn(ctx, "alex", sessionID, 1, "user", "I go to school yesterday", false, protocol.SourceText); err != nil {
		t.Fatalf("SaveTurn(user) error = %v", err)
	}
	if err := st.ReserveCorrectionJob(ctx, "alex", sessionID, 1); err != nil {
		t.Fatalf("ReserveCorrectionJob() error = %v", err)
	}

	status, err := st.JobStatus(ctx, "alex", sessionID, 1, "correction")
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
	var user *Turn
	for i := range turns {
		if turns[i].Turn == 1 && turns[i].Role == "user" {
			user = &turns[i]
		}
	}
	if user == nil {
		t.Fatalf("no user turn found: %+v", turns)
	}
	if user.CorrectionStatus != JobStatusPending {
		t.Fatalf("CorrectionStatus = %q, want %q", user.CorrectionStatus, JobStatusPending)
	}
	if user.Correction != nil {
		t.Fatalf("Correction = %+v, want nil before the job finishes", user.Correction)
	}
}

// TestMySQLSaveCorrectionMarksJobDone guards the atomicity SaveCorrection now
// gives the correction job status, mirroring CompleteAssistantTurn: a poller
// must never see CorrectionStatus == done before the Correction it belongs to
// is actually readable, and it must not need a reservation to still work
// (backward-compatible with callers/tests that save a correction directly).
func TestMySQLSaveCorrectionMarksJobDone(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()
	sessionID := "sess-save-correction-done"
	if err := st.SaveTurn(ctx, "alex", sessionID, 1, "user", "I go to school yesterday", false, protocol.SourceText); err != nil {
		t.Fatalf("SaveTurn(user) error = %v", err)
	}
	if err := st.ReserveCorrectionJob(ctx, "alex", sessionID, 1); err != nil {
		t.Fatalf("ReserveCorrectionJob() error = %v", err)
	}
	c := protocol.Correction{
		Original:  "I go to school yesterday",
		Corrected: "I went to school yesterday",
		Issues: []protocol.Issue{
			{Type: "grammar", Span: "go", Suggestion: "went", Explanation: "past time needs past tense"},
		},
	}
	if err := st.SaveCorrection(ctx, "alex", sessionID, 1, c); err != nil {
		t.Fatalf("SaveCorrection() error = %v", err)
	}

	status, err := st.JobStatus(ctx, "alex", sessionID, 1, "correction")
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
		if tn.Turn == 1 && tn.Role == "user" {
			found = true
			if tn.CorrectionStatus != JobStatusDone {
				t.Fatalf("CorrectionStatus = %q, want %q", tn.CorrectionStatus, JobStatusDone)
			}
			if tn.Correction == nil || tn.Correction.Corrected != "I went to school yesterday" {
				t.Fatalf("Correction = %+v, want the saved correction", tn.Correction)
			}
		}
	}
	if !found {
		t.Fatalf("no user turn found: %+v", turns)
	}
}
