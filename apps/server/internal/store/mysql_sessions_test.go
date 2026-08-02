package store

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"buddy/server/internal/llm"
	"buddy/server/internal/protocol"
)

func TestMySQLLoadUnknownUserReturnsZeroProfile(t *testing.T) {
	st := requireStore(t)
	got, err := st.Load(context.Background(), "nobody", "no-such-session")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got.Summary != "" || len(got.Recent) != 0 {
		t.Fatalf("expected zero Profile, got %+v", got)
	}
}

// TestMySQLSaveIsNoopWithoutExistingSessionRow: Save (unlike SaveTurn) is a
// plain UPDATE, not an upsert — a session row only exists once SaveTurn has
// created it. A connection that never sent a message must not leave a
// phantom row behind for ListSessions to show.
func TestMySQLSaveIsNoopWithoutExistingSessionRow(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()
	if err := st.Save(ctx, "ghost-user", "ghost-session", Profile{Summary: "should not stick"}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	got, err := st.Load(ctx, "ghost-user", "ghost-session")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got.Summary != "" || len(got.Recent) != 0 {
		t.Fatalf("expected Save() before any turn to be a no-op, got %+v", got)
	}
}

func TestMySQLSaveThenLoadRoundTrips(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()
	sessionID := "sess-round-trip"
	if err := st.SaveTurn(ctx, "alex", sessionID, 1, "user", "hi", false, protocol.SourceText); err != nil {
		t.Fatalf("SaveTurn() error = %v", err)
	}
	want := Profile{
		Summary: "likes hiking",
		Recent: []llm.Message{
			{Role: llm.RoleUser, Content: "hi"},
			{Role: llm.RoleAssistant, Content: "hello"},
		},
	}
	if err := st.Save(ctx, "alex", sessionID, want); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	got, err := st.Load(ctx, "alex", sessionID)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Load() = %+v, want %+v", got, want)
	}
}

func TestMySQLSaveTwiceUpdatesInPlace(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()
	sessionID := "sess-twice"
	if err := st.SaveTurn(ctx, "alex", sessionID, 1, "user", "hi", false, protocol.SourceText); err != nil {
		t.Fatalf("SaveTurn() error = %v", err)
	}
	if err := st.Save(ctx, "alex", sessionID, Profile{Summary: "v1"}); err != nil {
		t.Fatalf("Save() #1 error = %v", err)
	}
	if err := st.Save(ctx, "alex", sessionID, Profile{Summary: "v2"}); err != nil {
		t.Fatalf("Save() #2 error = %v", err)
	}

	got, err := st.Load(ctx, "alex", sessionID)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got.Summary != "v2" {
		t.Fatalf("Summary = %q, want v2 (update should overwrite)", got.Summary)
	}

	var n int
	if err := st.rw.QueryRowContext(ctx, `SELECT count(*) FROM `+sessionsTable+` WHERE user_id = ? AND id = ?`, "alex", sessionID).Scan(&n); err != nil {
		t.Fatalf("count query: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected exactly 1 row, got %d", n)
	}
}

// TestMySQLUsersAreIsolated uses the *same* session ID for two different
// users on purpose — the isolation must come from the composite
// (user_id, id) key, not from session IDs happening to differ.
func TestMySQLUsersAreIsolated(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()
	const sessionID = "s1"
	if err := st.SaveTurn(ctx, "alex", sessionID, 1, "user", "hi", false, protocol.SourceText); err != nil {
		t.Fatalf("SaveTurn(alex) error = %v", err)
	}
	if err := st.SaveTurn(ctx, "sam", sessionID, 1, "user", "hi", false, protocol.SourceText); err != nil {
		t.Fatalf("SaveTurn(sam) error = %v", err)
	}
	if err := st.Save(ctx, "alex", sessionID, Profile{Summary: "alex's memory"}); err != nil {
		t.Fatalf("Save(alex) error = %v", err)
	}
	if err := st.Save(ctx, "sam", sessionID, Profile{Summary: "sam's memory"}); err != nil {
		t.Fatalf("Save(sam) error = %v", err)
	}

	a, err := st.Load(ctx, "alex", sessionID)
	if err != nil {
		t.Fatalf("Load(alex) error = %v", err)
	}
	s, err := st.Load(ctx, "sam", sessionID)
	if err != nil {
		t.Fatalf("Load(sam) error = %v", err)
	}
	if a.Summary != "alex's memory" || s.Summary != "sam's memory" {
		t.Fatalf("cross-contamination between users: alex=%+v sam=%+v", a, s)
	}
}

func TestMySQLSaveGeneratedTitleOverwritesPlaceholder(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()
	sessionID := "sess-generated-title"
	if err := st.SaveTurn(ctx, "alex", sessionID, 1, "user", "first message", false, protocol.SourceText); err != nil {
		t.Fatalf("SaveTurn() error = %v", err)
	}
	if err := st.SaveGeneratedTitle(ctx, "alex", sessionID, "Hiking Trip Plans"); err != nil {
		t.Fatalf("SaveGeneratedTitle() error = %v", err)
	}
	meta, _, err := st.SessionDetail(ctx, "alex", sessionID)
	if err != nil {
		t.Fatalf("SessionDetail() error = %v", err)
	}
	if meta.Title != "Hiking Trip Plans" {
		t.Fatalf("Title = %q, want the generated title", meta.Title)
	}
}

// TestMySQLSaveGeneratedTitleOverwritesOnRegeneration guards the reason
// SaveGeneratedTitle unconditionally overwrites title rather than gating on
// title_generated the way it used to: internal/transport now calls this
// periodically as a conversation continues (see TitleRegenerateEveryNTurns),
// not just once on turn 1, and each call is expected to actually update the
// title to track the conversation's current topic.
func TestMySQLSaveGeneratedTitleOverwritesOnRegeneration(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()
	sessionID := "sess-generated-title-regen"
	if err := st.SaveTurn(ctx, "alex", sessionID, 1, "user", "first message", false, protocol.SourceText); err != nil {
		t.Fatalf("SaveTurn() error = %v", err)
	}
	if err := st.SaveGeneratedTitle(ctx, "alex", sessionID, "First Title"); err != nil {
		t.Fatalf("SaveGeneratedTitle(1) error = %v", err)
	}
	if err := st.SaveGeneratedTitle(ctx, "alex", sessionID, "Second Title"); err != nil {
		t.Fatalf("SaveGeneratedTitle(2) error = %v", err)
	}
	meta, _, err := st.SessionDetail(ctx, "alex", sessionID)
	if err != nil {
		t.Fatalf("SessionDetail() error = %v", err)
	}
	if meta.Title != "Second Title" {
		t.Fatalf("Title = %q, want the later regenerated title to win", meta.Title)
	}
}

// TestMySQLSaveGeneratedTitleCreatesRowWhenSessionMissing guards the fix for
// a real production race: Handler.generateTitle (ws.go) and the turn-1
// SaveTurn call that normally creates a session's row are fired off two
// independent, unsynchronized `go` statements, with no ordering guarantee
// between them. SaveGeneratedTitle must create the row itself when it wins
// that race — a plain no-op here would silently and permanently lose the
// generated title, since nothing else retries it.
func TestMySQLSaveGeneratedTitleCreatesRowWhenSessionMissing(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()
	sessionID := "sess-missing-for-title"
	if err := st.SaveGeneratedTitle(ctx, "alex", sessionID, "Some Title"); err != nil {
		t.Fatalf("SaveGeneratedTitle() error = %v", err)
	}
	meta, _, err := st.SessionDetail(ctx, "alex", sessionID)
	if err != nil {
		t.Fatalf("SessionDetail() error = %v", err)
	}
	if meta.Title != "Some Title" {
		t.Fatalf("Title = %q, want the generated title even though no turn had been saved yet", meta.Title)
	}
}

// TestMySQLSaveGeneratedTitleSurvivesLaterSaveTurn is the other half of the
// race TestMySQLSaveGeneratedTitleCreatesRowWhenSessionMissing covers: once
// SaveGeneratedTitle has won the race and created the row, the turn-1
// SaveTurn call that eventually does run must not clobber the generated
// title back to the raw-text placeholder.
func TestMySQLSaveGeneratedTitleSurvivesLaterSaveTurn(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()
	sessionID := "sess-title-race"
	if err := st.SaveGeneratedTitle(ctx, "alex", sessionID, "Generated Title"); err != nil {
		t.Fatalf("SaveGeneratedTitle() error = %v", err)
	}
	if err := st.SaveTurn(ctx, "alex", sessionID, 1, "user", "first message", false, protocol.SourceText); err != nil {
		t.Fatalf("SaveTurn() error = %v", err)
	}
	meta, _, err := st.SessionDetail(ctx, "alex", sessionID)
	if err != nil {
		t.Fatalf("SessionDetail() error = %v", err)
	}
	if meta.Title != "Generated Title" {
		t.Fatalf("Title = %q, want the already-generated title to survive the later turn-1 save", meta.Title)
	}
}

func TestMySQLSessionDetailOrdersUserBeforeAssistantWithinATurn(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()
	sessionID := "sess-order"
	if err := st.SaveTurn(ctx, "alex", sessionID, 1, "user", "user text", false, protocol.SourceText); err != nil {
		t.Fatalf("SaveTurn(user) error = %v", err)
	}
	if err := st.SaveTurn(ctx, "alex", sessionID, 1, "assistant", "assistant text", false, ""); err != nil {
		t.Fatalf("SaveTurn(assistant) error = %v", err)
	}
	_, turns, err := st.SessionDetail(ctx, "alex", sessionID)
	if err != nil {
		t.Fatalf("SessionDetail() error = %v", err)
	}
	if len(turns) != 2 || turns[0].Role != "user" || turns[1].Role != "assistant" {
		t.Fatalf("turns = %+v, want [user, assistant]", turns)
	}
}

func TestMySQLSessionDetailIncludesTurnCreatedAt(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()
	sessionID := "sess-created-at"
	before := time.Now().Unix()
	if err := st.SaveTurn(ctx, "alex", sessionID, 1, "user", "hi", false, protocol.SourceText); err != nil {
		t.Fatalf("SaveTurn() error = %v", err)
	}
	after := time.Now().Unix()

	_, turns, err := st.SessionDetail(ctx, "alex", sessionID)
	if err != nil {
		t.Fatalf("SessionDetail() error = %v", err)
	}
	if len(turns) != 1 {
		t.Fatalf("turns = %+v, want 1 turn", turns)
	}
	if turns[0].CreatedAt < before || turns[0].CreatedAt > after {
		t.Fatalf("turns[0].CreatedAt = %d, want between %d and %d", turns[0].CreatedAt, before, after)
	}
}

func TestMySQLSessionDetailCreatedAtSurvivesRefinedUpsert(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()
	sessionID := "sess-created-at-refined"
	if err := st.SaveTurn(ctx, "alex", sessionID, 1, "user", "i are hungry", false, protocol.SourceVoice); err != nil {
		t.Fatalf("SaveTurn(fast) error = %v", err)
	}
	_, before, err := st.SessionDetail(ctx, "alex", sessionID)
	if err != nil {
		t.Fatalf("SessionDetail() error = %v", err)
	}

	time.Sleep(1100 * time.Millisecond) // created_at has 1-second resolution (UNIX_TIMESTAMP())
	if err := st.SaveTurn(ctx, "alex", sessionID, 1, "user", "I am hungry", true, protocol.SourceVoice); err != nil {
		t.Fatalf("SaveTurn(refined) error = %v", err)
	}
	_, after, err := st.SessionDetail(ctx, "alex", sessionID)
	if err != nil {
		t.Fatalf("SessionDetail() error = %v", err)
	}

	if len(before) != 1 || len(after) != 1 {
		t.Fatalf("before = %+v, after = %+v, want 1 turn each", before, after)
	}
	if after[0].CreatedAt != before[0].CreatedAt {
		t.Fatalf("CreatedAt changed on refine upsert: before = %d, after = %d", before[0].CreatedAt, after[0].CreatedAt)
	}
}

// seedTurns saves a user+assistant pair for each of turns 1..n in sessionID,
// for the pagination tests below — each pair is one "turn" as the frontend's
// scroll-back page cursor counts them.
func seedTurns(t *testing.T, st *MySQLStore, userID, sessionID string, n int) {
	t.Helper()
	ctx := context.Background()
	for turn := 1; turn <= n; turn++ {
		if err := st.SaveTurn(ctx, userID, sessionID, turn, "user", fmt.Sprintf("user turn %d", turn), false, protocol.SourceText); err != nil {
			t.Fatalf("SaveTurn(user, %d) error = %v", turn, err)
		}
		if err := st.SaveTurn(ctx, userID, sessionID, turn, "assistant", fmt.Sprintf("assistant turn %d", turn), false, ""); err != nil {
			t.Fatalf("SaveTurn(assistant, %d) error = %v", turn, err)
		}
	}
}

func TestMySQLSessionDetailPageReturnsMostRecentTurnsFirstAndFlagsHasMore(t *testing.T) {
	st := requireStore(t)
	sessionID := "sess-page-latest"
	seedTurns(t, st, "alex", sessionID, 5)

	_, turns, hasMore, err := st.SessionDetailPage(context.Background(), "alex", sessionID, 0, 2)
	if err != nil {
		t.Fatalf("SessionDetailPage() error = %v", err)
	}
	if !hasMore {
		t.Fatalf("hasMore = false, want true (turns 1-3 still precede this page)")
	}
	wantTurns := []int{4, 4, 5, 5}
	if len(turns) != len(wantTurns) {
		t.Fatalf("turns = %+v, want 4 rows (turns 4 and 5, user+assistant each)", turns)
	}
	for i, want := range wantTurns {
		if turns[i].Turn != want {
			t.Fatalf("turns[%d].Turn = %d, want %d (turns = %+v)", i, turns[i].Turn, want, turns)
		}
	}
}

func TestMySQLSessionDetailPageHasMoreFalseWhenPageCoversWholeHistory(t *testing.T) {
	st := requireStore(t)
	sessionID := "sess-page-exact"
	seedTurns(t, st, "alex", sessionID, 3)

	_, turns, hasMore, err := st.SessionDetailPage(context.Background(), "alex", sessionID, 0, 3)
	if err != nil {
		t.Fatalf("SessionDetailPage() error = %v", err)
	}
	if hasMore {
		t.Fatalf("hasMore = true, want false (exactly 3 turns exist and the page holds all 3)")
	}
	if len(turns) != 6 {
		t.Fatalf("turns = %+v, want 6 rows (3 turns, user+assistant each)", turns)
	}
}

func TestMySQLSessionDetailPageCursorWalksBackThroughOlderTurns(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()
	sessionID := "sess-page-cursor"
	seedTurns(t, st, "alex", sessionID, 5)

	_, page1, hasMore1, err := st.SessionDetailPage(ctx, "alex", sessionID, 0, 2)
	if err != nil {
		t.Fatalf("SessionDetailPage(page1) error = %v", err)
	}
	if !hasMore1 || len(page1) != 4 || page1[0].Turn != 4 {
		t.Fatalf("page1 = %+v, hasMore1 = %v, want turns [4,4,5,5] with hasMore1 = true", page1, hasMore1)
	}

	_, page2, hasMore2, err := st.SessionDetailPage(ctx, "alex", sessionID, page1[0].Turn, 2)
	if err != nil {
		t.Fatalf("SessionDetailPage(page2) error = %v", err)
	}
	if !hasMore2 || len(page2) != 4 || page2[0].Turn != 2 {
		t.Fatalf("page2 = %+v, hasMore2 = %v, want turns [2,2,3,3] with hasMore2 = true", page2, hasMore2)
	}

	_, page3, hasMore3, err := st.SessionDetailPage(ctx, "alex", sessionID, page2[0].Turn, 2)
	if err != nil {
		t.Fatalf("SessionDetailPage(page3) error = %v", err)
	}
	if hasMore3 || len(page3) != 2 || page3[0].Turn != 1 {
		t.Fatalf("page3 = %+v, hasMore3 = %v, want turn [1,1] with hasMore3 = false (nothing older left)", page3, hasMore3)
	}
}

func TestMySQLSessionDetailPageNonexistentSessionReturnsErrNotFound(t *testing.T) {
	st := requireStore(t)
	_, turns, hasMore, err := st.SessionDetailPage(context.Background(), "alex", "sess-page-nonexistent", 0, 2)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("SessionDetailPage() error = %v, want ErrNotFound for a session that was never created", err)
	}
	if hasMore || turns != nil {
		t.Fatalf("turns = %+v, hasMore = %v, want nil/false on ErrNotFound", turns, hasMore)
	}
}

func TestMySQLListSessionsOrderedByRecency(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()
	userID := "list-order-user"
	if err := st.SaveTurn(ctx, userID, "s-old", 1, "user", "older room", false, protocol.SourceText); err != nil {
		t.Fatalf("SaveTurn(old) error = %v", err)
	}
	time.Sleep(1100 * time.Millisecond) // updated_at has 1-second resolution (UNIX_TIMESTAMP())
	if err := st.SaveTurn(ctx, userID, "s-new", 1, "user", "newer room", false, protocol.SourceText); err != nil {
		t.Fatalf("SaveTurn(new) error = %v", err)
	}

	sessions, err := st.ListSessions(ctx, userID)
	if err != nil {
		t.Fatalf("ListSessions() error = %v", err)
	}
	if len(sessions) != 2 || sessions[0].ID != "s-new" || sessions[1].ID != "s-old" {
		t.Fatalf("ListSessions() = %+v, want [s-new, s-old]", sessions)
	}
}

func TestMySQLDeleteSessionRemovesSessionAndTurns(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()
	sessionID := "sess-delete"
	if err := st.SaveTurn(ctx, "alex", sessionID, 1, "user", "hi", false, protocol.SourceText); err != nil {
		t.Fatalf("SaveTurn() error = %v", err)
	}
	if err := st.DeleteSession(ctx, "alex", sessionID); err != nil {
		t.Fatalf("DeleteSession() error = %v", err)
	}

	if _, _, err := st.SessionDetail(ctx, "alex", sessionID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("SessionDetail() after delete error = %v, want ErrNotFound", err)
	}
	var n int
	if err := st.rw.QueryRowContext(ctx, `SELECT count(*) FROM `+turnsTable+` WHERE user_id = ? AND session_id = ?`, "alex", sessionID).Scan(&n); err != nil {
		t.Fatalf("count query: %v", err)
	}
	if n != 0 {
		t.Fatalf("expected buddy_turns rows to be gone, found %d", n)
	}
}

func TestMySQLDeleteSessionIsNoopForUnknownSession(t *testing.T) {
	st := requireStore(t)
	if err := st.DeleteSession(context.Background(), "alex", "no-such-session"); err != nil {
		t.Fatalf("DeleteSession() error = %v, want nil (no-op)", err)
	}
}

// TestMySQLDeleteSessionDoesNotAffectOtherUsers guards the same isolation
// property as TestMySQLUsersAreIsolated, but for deletion: a delete scoped to
// one user must never touch another user's row for the same session ID.
func TestMySQLDeleteSessionDoesNotAffectOtherUsers(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()
	const sessionID = "s-shared-delete"
	if err := st.SaveTurn(ctx, "victim", sessionID, 1, "user", "victim's message", false, protocol.SourceText); err != nil {
		t.Fatalf("SaveTurn(victim) error = %v", err)
	}
	if err := st.SaveTurn(ctx, "attacker", sessionID, 1, "user", "attacker's message", false, protocol.SourceText); err != nil {
		t.Fatalf("SaveTurn(attacker) error = %v", err)
	}
	if err := st.DeleteSession(ctx, "attacker", sessionID); err != nil {
		t.Fatalf("DeleteSession(attacker) error = %v", err)
	}
	if _, _, err := st.SessionDetail(ctx, "victim", sessionID); err != nil {
		t.Fatalf("victim's session should survive attacker's delete, SessionDetail() error = %v", err)
	}
}

// TestMySQLEndSessionFreezesImmediatelyWithoutTheSummary guards the async
// wrap-up flow: EndSession alone must mark the room Ended and its
// StudySummaryStatus JobStatusPending, with StudySummary still empty — the
// LLM call hasn't run yet at this point, only CompleteStudySummary (below)
// fills it in, from the background asyncjob.KindStudySummary job.
func TestMySQLEndSessionFreezesImmediatelyWithoutTheSummary(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()
	// A dedicated userID, not the heavily-shared "alex" other tests in this
	// file reuse — ListSessions below must see only this test's own room.
	const userID = "end-session-user"
	sessionID := "sess-end"
	if err := st.SaveTurn(ctx, userID, sessionID, 1, "user", "first message", false, protocol.SourceText); err != nil {
		t.Fatalf("SaveTurn() error = %v", err)
	}
	if err := st.EndSession(ctx, userID, sessionID); err != nil {
		t.Fatalf("EndSession() error = %v", err)
	}
	meta, _, err := st.SessionDetail(ctx, userID, sessionID)
	if err != nil {
		t.Fatalf("SessionDetail() error = %v", err)
	}
	if !meta.Ended {
		t.Fatalf("Ended = false, want true after EndSession")
	}
	if meta.StudySummaryStatus != JobStatusPending {
		t.Fatalf("StudySummaryStatus = %q, want JobStatusPending immediately after EndSession", meta.StudySummaryStatus)
	}
	if len(meta.StudySummary) != 0 {
		t.Fatalf("StudySummary = %+v, want empty until CompleteStudySummary runs", meta.StudySummary)
	}
	// EndSession flips quiz_status to pending in the same UPDATE as
	// study_summary_status — the two background jobs (asyncjob.
	// KindStudySummary/KindStudyQuiz) are enqueued together right after this
	// by httpserver.sessionEndHandler, not one after the other.
	if meta.QuizStatus != JobStatusPending {
		t.Fatalf("QuizStatus = %q, want JobStatusPending immediately after EndSession", meta.QuizStatus)
	}
	if len(meta.Quiz) != 0 {
		t.Fatalf("Quiz = %+v, want empty until CompleteStudyQuiz runs", meta.Quiz)
	}

	sessions, err := st.ListSessions(ctx, userID)
	if err != nil {
		t.Fatalf("ListSessions() error = %v", err)
	}
	if len(sessions) != 1 || !sessions[0].Ended || sessions[0].StudySummaryStatus != JobStatusPending || sessions[0].QuizStatus != JobStatusPending {
		t.Fatalf("ListSessions() = %+v, want the ended session pending its wrap-up and quiz", sessions)
	}
}

// TestMySQLEndSessionMissingSessionIsNoop mirrors Save's "no row to match" —
// EndSession never creates a session row on its own, only the confirm button
// flow from an already-open room does.
func TestMySQLEndSessionMissingSessionIsNoop(t *testing.T) {
	st := requireStore(t)
	if err := st.EndSession(context.Background(), "alex", "no-such-session"); err != nil {
		t.Fatalf("EndSession() error = %v, want nil (silent no-op)", err)
	}
}

// TestMySQLSessionDetailNotFoundForWrongUser is the key security property of
// the composite-key schema: a session ID guessed or leaked from another user
// must not be readable, even though the ID itself exists in the table.
func TestMySQLSessionDetailNotFoundForWrongUser(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()
	sessionID := "shared-id-guess"
	if err := st.SaveTurn(ctx, "victim", sessionID, 1, "user", "victim's secret message", false, protocol.SourceText); err != nil {
		t.Fatalf("SaveTurn() error = %v", err)
	}
	_, _, err := st.SessionDetail(ctx, "attacker", sessionID)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("SessionDetail(attacker, victim's session) error = %v, want ErrNotFound", err)
	}
	attackerSessions, err := st.ListSessions(ctx, "attacker")
	if err != nil {
		t.Fatalf("ListSessions(attacker) error = %v", err)
	}
	for _, s := range attackerSessions {
		if s.ID == sessionID {
			t.Fatalf("attacker's session list leaked victim's session: %+v", attackerSessions)
		}
	}
}
