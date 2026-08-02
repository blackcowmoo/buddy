package store

import (
	"context"
	"testing"
)

func TestMySQLGetInterlocutorStyleUnknownUserReturnsEmptyString(t *testing.T) {
	st := requireStore(t)
	got, err := st.GetInterlocutorStyle(context.Background(), "no-such-user")
	if err != nil {
		t.Fatalf("GetInterlocutorStyle() error = %v", err)
	}
	if got != "" {
		t.Fatalf("GetInterlocutorStyle() = %q, want \"\" for a user with no saved setting", got)
	}
}

func TestMySQLSaveInterlocutorStyleThenGetRoundTrips(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()
	const userID = "style-user"
	if err := st.SaveInterlocutorStyle(ctx, userID, "ask interview-style questions"); err != nil {
		t.Fatalf("SaveInterlocutorStyle() error = %v", err)
	}
	got, err := st.GetInterlocutorStyle(ctx, userID)
	if err != nil {
		t.Fatalf("GetInterlocutorStyle() error = %v", err)
	}
	if got != "ask interview-style questions" {
		t.Fatalf("GetInterlocutorStyle() = %q, want the saved value", got)
	}
}

// TestMySQLSaveInterlocutorStyleTwiceOverwrites guards the upsert: a second
// save for the same user must replace the first, not add a second row (which
// would make GetInterlocutorStyle's single-row SELECT ambiguous).
func TestMySQLSaveInterlocutorStyleTwiceOverwrites(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()
	const userID = "style-user-overwrite"
	if err := st.SaveInterlocutorStyle(ctx, userID, "v1"); err != nil {
		t.Fatalf("SaveInterlocutorStyle() #1 error = %v", err)
	}
	if err := st.SaveInterlocutorStyle(ctx, userID, "v2"); err != nil {
		t.Fatalf("SaveInterlocutorStyle() #2 error = %v", err)
	}
	got, err := st.GetInterlocutorStyle(ctx, userID)
	if err != nil {
		t.Fatalf("GetInterlocutorStyle() error = %v", err)
	}
	if got != "v2" {
		t.Fatalf("GetInterlocutorStyle() = %q, want v2 (overwrite)", got)
	}
	var n int
	if err := st.rw.QueryRowContext(ctx, `SELECT count(*) FROM `+settingsTable+` WHERE user_id = ?`, userID).Scan(&n); err != nil {
		t.Fatalf("count query: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected exactly 1 row, got %d", n)
	}
}

// TestMySQLInterlocutorStylesAreIsolated guards the same per-user isolation
// property as TestMySQLUsersAreIsolated, for the settings table.
func TestMySQLInterlocutorStylesAreIsolated(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()
	if err := st.SaveInterlocutorStyle(ctx, "style-alex", "alex's style"); err != nil {
		t.Fatalf("SaveInterlocutorStyle(alex) error = %v", err)
	}
	if err := st.SaveInterlocutorStyle(ctx, "style-sam", "sam's style"); err != nil {
		t.Fatalf("SaveInterlocutorStyle(sam) error = %v", err)
	}
	a, err := st.GetInterlocutorStyle(ctx, "style-alex")
	if err != nil {
		t.Fatalf("GetInterlocutorStyle(alex) error = %v", err)
	}
	s, err := st.GetInterlocutorStyle(ctx, "style-sam")
	if err != nil {
		t.Fatalf("GetInterlocutorStyle(sam) error = %v", err)
	}
	if a != "alex's style" || s != "sam's style" {
		t.Fatalf("cross-contamination between users: alex=%q sam=%q", a, s)
	}
}

func TestMySQLGetLearnerProfileUnknownUserReturnsEmptyString(t *testing.T) {
	st := requireStore(t)
	got, err := st.GetLearnerProfile(context.Background(), "no-such-user")
	if err != nil {
		t.Fatalf("GetLearnerProfile() error = %v", err)
	}
	if got != "" {
		t.Fatalf("GetLearnerProfile() = %q, want \"\" for a user with no saved profile", got)
	}
}

func TestMySQLSaveLearnerProfileThenGetRoundTrips(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()
	const userID = "profile-user"
	if err := st.SaveLearnerProfile(ctx, userID, "struggles with third-person -s"); err != nil {
		t.Fatalf("SaveLearnerProfile() error = %v", err)
	}
	got, err := st.GetLearnerProfile(ctx, userID)
	if err != nil {
		t.Fatalf("GetLearnerProfile() error = %v", err)
	}
	if got != "struggles with third-person -s" {
		t.Fatalf("GetLearnerProfile() = %q, want the saved value", got)
	}
}

// TestMySQLSaveLearnerProfileTwiceOverwrites guards the upsert the same way
// TestMySQLSaveInterlocutorStyleTwiceOverwrites does for its sibling column.
func TestMySQLSaveLearnerProfileTwiceOverwrites(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()
	const userID = "profile-user-overwrite"
	if err := st.SaveLearnerProfile(ctx, userID, "v1"); err != nil {
		t.Fatalf("SaveLearnerProfile() #1 error = %v", err)
	}
	if err := st.SaveLearnerProfile(ctx, userID, "v2"); err != nil {
		t.Fatalf("SaveLearnerProfile() #2 error = %v", err)
	}
	got, err := st.GetLearnerProfile(ctx, userID)
	if err != nil {
		t.Fatalf("GetLearnerProfile() error = %v", err)
	}
	if got != "v2" {
		t.Fatalf("GetLearnerProfile() = %q, want v2 (overwrite)", got)
	}
	var n int
	if err := st.rw.QueryRowContext(ctx, `SELECT count(*) FROM `+settingsTable+` WHERE user_id = ?`, userID).Scan(&n); err != nil {
		t.Fatalf("count query: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected exactly 1 row, got %d", n)
	}
}

// TestMySQLSaveLearnerProfileBeforeInterlocutorStyleDoesNotViolateNotNull
// guards SaveLearnerProfile's explicit interlocutor_style = ” on first
// insert: that column has no column-level default, so a learner ending a
// conversation before ever visiting Settings must not trip a NOT NULL
// violation under MySQL's strict mode.
func TestMySQLSaveLearnerProfileBeforeInterlocutorStyleDoesNotViolateNotNull(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()
	const userID = "profile-before-style"
	if err := st.SaveLearnerProfile(ctx, userID, "first profile"); err != nil {
		t.Fatalf("SaveLearnerProfile() error = %v", err)
	}
	style, err := st.GetInterlocutorStyle(ctx, userID)
	if err != nil {
		t.Fatalf("GetInterlocutorStyle() error = %v", err)
	}
	if style != "" {
		t.Fatalf("GetInterlocutorStyle() = %q, want \"\" (untouched)", style)
	}
}
