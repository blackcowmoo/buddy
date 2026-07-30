package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	tcmysql "github.com/testcontainers/testcontainers-go/modules/mysql"
	"github.com/testcontainers/testcontainers-go/wait"

	"buddy/server/internal/llm"
	"buddy/server/internal/protocol"
)

// TestHostPort guards against a real incident: a secret store injected
// MYSQL_RW_HOSTNAME already as "host:port", and unconditionally appending
// cfg.Port turned it into a bracketed literal ("[host:port]:port") whose DNS
// lookup target was the whole "host:port" string — "no such host".
func TestHostPort(t *testing.T) {
	tests := []struct {
		name         string
		host         string
		fallbackPort int
		want         string
	}{
		{"bare hostname gets fallback port appended", "mysql-primary.internal", 3306, "mysql-primary.internal:3306"},
		{"host already carrying its own port is used as-is", "mysql-primary.internal:3307", 3306, "mysql-primary.internal:3307"},
		{"bare IPv4 gets fallback port appended", "10.0.0.5", 3306, "10.0.0.5:3306"},
		{"IPv4 with its own port is used as-is", "10.0.0.5:3307", 3306, "10.0.0.5:3307"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := HostPort(tt.host, tt.fallbackPort); got != tt.want {
				t.Errorf("HostPort(%q, %d) = %q, want %q", tt.host, tt.fallbackPort, got, tt.want)
			}
		})
	}
}

// sharedStore backs every container test in this file. It's started once in
// TestMain rather than per-test: a query-level mock wouldn't have caught the
// SQLite busy-timeout bug this project already hit once — real SQL semantics
// (the ON DUPLICATE KEY upsert, the utf8mb4 schema, VARCHAR-vs-TEXT key
// limits, connection handling) need a real database, but a fresh container
// per test quadruples this file's CI time for no isolation benefit: every
// test below upserts by primary key, so each test's own writes fully
// determine the state its own reads observe regardless of what earlier tests
// left behind.
var (
	sharedStore    *MySQLStore
	sharedStoreErr error
	// sharedStoreConfig is reused by tests that need to reconnect to the same
	// already-migrated container, e.g. TestMySQLNewMySQLIsIdempotent.
	sharedStoreConfig MySQLConfig
)

func TestMain(m *testing.M) {
	os.Exit(runMySQLTests(m))
}

func runMySQLTests(m *testing.M) int {
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	container, err := tcmysql.Run(ctx, "mysql:8.0",
		tcmysql.WithDatabase("buddy"),
		tcmysql.WithUsername("buddy"),
		tcmysql.WithPassword("buddy"),
		// Same readiness log line the module picks by default, just with more
		// patience for a slow first boot on CI.
		testcontainers.WithWaitStrategy(
			wait.ForLog("port: 3306  MySQL Community Server").
				WithStartupTimeout(120*time.Second),
		),
	)
	if err != nil {
		// No/unreachable Docker (sandboxed CI, restricted dev box): record why
		// so container tests skip themselves instead of failing the suite, but
		// still run the pure-function tests like TestHostPort.
		sharedStoreErr = err
		return m.Run()
	}
	defer func() { _ = container.Terminate(context.Background()) }()

	host, err := container.Host(ctx)
	if err != nil {
		sharedStoreErr = err
		return m.Run()
	}
	port, err := container.MappedPort(ctx, "3306/tcp")
	if err != nil {
		sharedStoreErr = err
		return m.Run()
	}

	sharedStoreConfig = MySQLConfig{
		RWHost:   host,
		Port:     int(port.Num()),
		User:     "buddy",
		Password: "buddy",
		Database: "buddy",
	}
	st, err := NewMySQL(sharedStoreConfig)
	if err != nil {
		sharedStoreErr = err
		return m.Run()
	}
	defer func() { _ = st.Close() }()

	sharedStore = st
	return m.Run()
}

func requireStore(t *testing.T) *MySQLStore {
	t.Helper()
	if sharedStoreErr != nil {
		t.Skipf("mysql testcontainer unavailable (no/unreachable Docker?): %v", sharedStoreErr)
	}
	return sharedStore
}

// TestMySQLNewMySQLIsIdempotent guards a real incident this test caught: the
// additive "translation" column migration originally used MariaDB-only
// `ADD COLUMN IF NOT EXISTS` syntax, which is a syntax error on real MySQL
// and made every boot after the first fail with "store: schema: ...". A
// second NewMySQL against an already-migrated database (as happens on every
// pod restart in production) must succeed, not just the first.
func TestMySQLNewMySQLIsIdempotent(t *testing.T) {
	requireStore(t) // ensures TestMain's container is up and already migrated

	st, err := NewMySQL(sharedStoreConfig)
	if err != nil {
		t.Fatalf("NewMySQL() on an already-migrated database: error = %v", err)
	}
	_ = st.Close()
}

// TestMySQLNewMySQLResetsLegacyPlainTextStudySummaries guards the migration
// that protects decodeStudySummary from a real hazard: a "done" study_summary
// row written before GenerateStudySummary switched to the bilingual JSON
// shape is plain native-language prose, not JSON — silently undecodable, not
// silently upgradable. A row already in the new JSON-array shape (starts
// with '[') must survive a re-migration untouched; only genuinely legacy
// plain-text rows get reset back to JobStatusPending — not "" — so
// httpserver.sessionDetailHandler's needsStudySummaryBackfill re-enqueues a
// regeneration job for it (see that function's doc comment) instead of the
// learner's real feedback just staying gone.
func TestMySQLNewMySQLResetsLegacyPlainTextStudySummaries(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()
	const userID = "legacy-summary-user"
	legacySession, jsonSession := "sess-legacy", "sess-json"

	if err := st.SaveTurn(ctx, userID, legacySession, 1, "user", "first message", false, protocol.SourceText); err != nil {
		t.Fatalf("SaveTurn(legacy) error = %v", err)
	}
	if err := st.SaveTurn(ctx, userID, jsonSession, 1, "user", "first message", false, protocol.SourceText); err != nil {
		t.Fatalf("SaveTurn(json) error = %v", err)
	}
	// legacySession gets the old plain-text shape written directly (as if by
	// a pre-migration CompleteStudySummary); jsonSession gets the real thing.
	if _, err := st.rw.ExecContext(ctx, `
		UPDATE `+sessionsTable+` SET study_summary = ?, study_summary_status = 'done' WHERE user_id = ? AND id = ?
	`, "그동안 3인칭 단수 -s를 자주 빠뜨렸어요.", userID, legacySession); err != nil {
		t.Fatalf("seed legacy study_summary: %v", err)
	}
	if err := st.EndSession(ctx, userID, jsonSession); err != nil {
		t.Fatalf("EndSession(json) error = %v", err)
	}
	wantJSON := []protocol.StudySummarySentence{{English: "Focus on third-person -s.", Translation: "3인칭 단수 -s에 집중하세요."}}
	if err := st.CompleteStudySummary(ctx, userID, jsonSession, wantJSON); err != nil {
		t.Fatalf("CompleteStudySummary(json) error = %v", err)
	}

	reopened, err := NewMySQL(sharedStoreConfig)
	if err != nil {
		t.Fatalf("NewMySQL() error = %v", err)
	}
	defer reopened.Close()

	legacyMeta, _, err := reopened.SessionDetail(ctx, userID, legacySession)
	if err != nil {
		t.Fatalf("SessionDetail(legacy) error = %v", err)
	}
	if legacyMeta.StudySummaryStatus != JobStatusPending || len(legacyMeta.StudySummary) != 0 {
		t.Fatalf("legacy meta = %+v, want the plain-text summary reset to empty with status pending-regeneration", legacyMeta)
	}

	jsonMeta, _, err := reopened.SessionDetail(ctx, userID, jsonSession)
	if err != nil {
		t.Fatalf("SessionDetail(json) error = %v", err)
	}
	if jsonMeta.StudySummaryStatus != JobStatusDone || len(jsonMeta.StudySummary) != 1 || jsonMeta.StudySummary[0] != wantJSON[0] {
		t.Fatalf("json meta = %+v, want the bilingual summary left untouched by the legacy-reset migration", jsonMeta)
	}
}

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

// TestMySQLCompleteStudySummarySavesTextAndMarksDone guards the background
// job's terminal write: once the asyncjob.KindStudySummary job finishes, its
// text lands in StudySummary and StudySummaryStatus flips to JobStatusDone —
// the state a reopened room stops polling on.
func TestMySQLCompleteStudySummarySavesTextAndMarksDone(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()
	const userID = "complete-summary-user"
	sessionID := "sess-complete"
	if err := st.SaveTurn(ctx, userID, sessionID, 1, "user", "first message", false, protocol.SourceText); err != nil {
		t.Fatalf("SaveTurn() error = %v", err)
	}
	if err := st.EndSession(ctx, userID, sessionID); err != nil {
		t.Fatalf("EndSession() error = %v", err)
	}
	wantSummary := []protocol.StudySummarySentence{{English: "Focus on third-person -s.", Translation: "3인칭 단수 -s에 집중하세요."}}
	if err := st.CompleteStudySummary(ctx, userID, sessionID, wantSummary); err != nil {
		t.Fatalf("CompleteStudySummary() error = %v", err)
	}
	meta, _, err := st.SessionDetail(ctx, userID, sessionID)
	if err != nil {
		t.Fatalf("SessionDetail() error = %v", err)
	}
	if meta.StudySummaryStatus != JobStatusDone {
		t.Fatalf("StudySummaryStatus = %q, want JobStatusDone", meta.StudySummaryStatus)
	}
	if len(meta.StudySummary) != 1 || meta.StudySummary[0] != wantSummary[0] {
		t.Fatalf("StudySummary = %+v, want %+v", meta.StudySummary, wantSummary)
	}
}

// TestMySQLFailStudySummaryMarksFailedWithoutTouchingText guards the error
// path: a failed generation attempt must record JobStatusFailed for a
// poller to show, without inventing any StudySummary text.
func TestMySQLFailStudySummaryMarksFailedWithoutTouchingText(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()
	const userID = "fail-summary-user"
	sessionID := "sess-fail"
	if err := st.SaveTurn(ctx, userID, sessionID, 1, "user", "first message", false, protocol.SourceText); err != nil {
		t.Fatalf("SaveTurn() error = %v", err)
	}
	if err := st.EndSession(ctx, userID, sessionID); err != nil {
		t.Fatalf("EndSession() error = %v", err)
	}
	if err := st.FailStudySummary(ctx, userID, sessionID); err != nil {
		t.Fatalf("FailStudySummary() error = %v", err)
	}
	meta, _, err := st.SessionDetail(ctx, userID, sessionID)
	if err != nil {
		t.Fatalf("SessionDetail() error = %v", err)
	}
	if meta.StudySummaryStatus != JobStatusFailed {
		t.Fatalf("StudySummaryStatus = %q, want JobStatusFailed", meta.StudySummaryStatus)
	}
	if len(meta.StudySummary) != 0 {
		t.Fatalf("StudySummary = %+v, want still empty after a failed attempt", meta.StudySummary)
	}
}

// TestMySQLRestartStudySummaryResetsDoneToPendingAndClearsSummary guards the
// manual "다시 확인하기" recovery path (see httpserver.sessionRestudyHandler):
// a wrap-up already landed as JobStatusDone — including one carrying real
// content — must go back to JobStatusPending with StudySummary cleared, so
// asyncjob.KindStudySummary regenerates it from scratch instead of the
// handler's guard (checked before this is ever called) being the only thing
// standing between a learner and silently wiping a good wrap-up.
func TestMySQLRestartStudySummaryResetsDoneToPendingAndClearsSummary(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()
	const userID = "restart-summary-user"
	sessionID := "sess-restart"
	if err := st.SaveTurn(ctx, userID, sessionID, 1, "user", "first message", false, protocol.SourceText); err != nil {
		t.Fatalf("SaveTurn() error = %v", err)
	}
	if err := st.EndSession(ctx, userID, sessionID); err != nil {
		t.Fatalf("EndSession() error = %v", err)
	}
	if err := st.CompleteStudySummary(ctx, userID, sessionID, nil); err != nil {
		t.Fatalf("CompleteStudySummary() error = %v", err)
	}

	if err := st.RestartStudySummary(ctx, userID, sessionID); err != nil {
		t.Fatalf("RestartStudySummary() error = %v", err)
	}
	meta, _, err := st.SessionDetail(ctx, userID, sessionID)
	if err != nil {
		t.Fatalf("SessionDetail() error = %v", err)
	}
	if meta.StudySummaryStatus != JobStatusPending || len(meta.StudySummary) != 0 {
		t.Fatalf("meta = %+v, want JobStatusPending with an empty summary", meta)
	}
}

// TestMySQLRestartStudySummaryMissingSessionIsNoop mirrors EndSession's "no
// row to match" behavior — RestartStudySummary never creates a session row
// on its own.
func TestMySQLRestartStudySummaryMissingSessionIsNoop(t *testing.T) {
	st := requireStore(t)
	if err := st.RestartStudySummary(context.Background(), "alex", "no-such-session"); err != nil {
		t.Fatalf("RestartStudySummary() error = %v, want nil (silent no-op)", err)
	}
}

// TestMySQLCompleteStudyQuizSavesQuestionsAndMarksDone mirrors
// TestMySQLCompleteStudySummarySavesTextAndMarksDone: once the
// asyncjob.KindStudyQuiz job finishes, its questions land in Quiz and
// QuizStatus flips to JobStatusDone.
func TestMySQLCompleteStudyQuizSavesQuestionsAndMarksDone(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()
	const userID = "complete-quiz-user"
	sessionID := "sess-complete-quiz"
	if err := st.SaveTurn(ctx, userID, sessionID, 1, "user", "first message", false, protocol.SourceText); err != nil {
		t.Fatalf("SaveTurn() error = %v", err)
	}
	if err := st.EndSession(ctx, userID, sessionID); err != nil {
		t.Fatalf("EndSession() error = %v", err)
	}
	wantQuiz := []protocol.QuizQuestion{{Prompt: "He ___ to school.", Answer: "goes", Translation: "그는 학교에 가요.", Explanation: "subject-verb agreement", ExplanationTranslation: "주어-동사 일치"}}
	if err := st.CompleteStudyQuiz(ctx, userID, sessionID, wantQuiz); err != nil {
		t.Fatalf("CompleteStudyQuiz() error = %v", err)
	}
	meta, _, err := st.SessionDetail(ctx, userID, sessionID)
	if err != nil {
		t.Fatalf("SessionDetail() error = %v", err)
	}
	if meta.QuizStatus != JobStatusDone {
		t.Fatalf("QuizStatus = %q, want JobStatusDone", meta.QuizStatus)
	}
	if len(meta.Quiz) != 1 || meta.Quiz[0] != wantQuiz[0] {
		t.Fatalf("Quiz = %+v, want %+v", meta.Quiz, wantQuiz)
	}
}

// TestMySQLCompleteStudyQuizWithNoQuestionsStillMarksDone documents that an
// ended session with no flagged issues (runStudyQuiz skips the LLM call
// entirely — see its doc comment) still reaches a terminal QuizStatus, with
// Quiz staying empty rather than the JSON literal "[]" — same round-trip
// contract as CompleteStudySummary's empty-summary case.
func TestMySQLCompleteStudyQuizWithNoQuestionsStillMarksDone(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()
	const userID = "complete-quiz-empty-user"
	sessionID := "sess-complete-quiz-empty"
	if err := st.SaveTurn(ctx, userID, sessionID, 1, "user", "first message", false, protocol.SourceText); err != nil {
		t.Fatalf("SaveTurn() error = %v", err)
	}
	if err := st.EndSession(ctx, userID, sessionID); err != nil {
		t.Fatalf("EndSession() error = %v", err)
	}
	if err := st.CompleteStudyQuiz(ctx, userID, sessionID, nil); err != nil {
		t.Fatalf("CompleteStudyQuiz() error = %v", err)
	}
	meta, _, err := st.SessionDetail(ctx, userID, sessionID)
	if err != nil {
		t.Fatalf("SessionDetail() error = %v", err)
	}
	if meta.QuizStatus != JobStatusDone {
		t.Fatalf("QuizStatus = %q, want JobStatusDone", meta.QuizStatus)
	}
	if len(meta.Quiz) != 0 {
		t.Fatalf("Quiz = %+v, want empty", meta.Quiz)
	}
}

// TestMySQLFailStudyQuizMarksFailedWithoutTouchingQuestions mirrors
// TestMySQLFailStudySummaryMarksFailedWithoutTouchingText.
func TestMySQLFailStudyQuizMarksFailedWithoutTouchingQuestions(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()
	const userID = "fail-quiz-user"
	sessionID := "sess-fail-quiz"
	if err := st.SaveTurn(ctx, userID, sessionID, 1, "user", "first message", false, protocol.SourceText); err != nil {
		t.Fatalf("SaveTurn() error = %v", err)
	}
	if err := st.EndSession(ctx, userID, sessionID); err != nil {
		t.Fatalf("EndSession() error = %v", err)
	}
	if err := st.FailStudyQuiz(ctx, userID, sessionID); err != nil {
		t.Fatalf("FailStudyQuiz() error = %v", err)
	}
	meta, _, err := st.SessionDetail(ctx, userID, sessionID)
	if err != nil {
		t.Fatalf("SessionDetail() error = %v", err)
	}
	if meta.QuizStatus != JobStatusFailed {
		t.Fatalf("QuizStatus = %q, want JobStatusFailed", meta.QuizStatus)
	}
	if len(meta.Quiz) != 0 {
		t.Fatalf("Quiz = %+v, want still empty after a failed attempt", meta.Quiz)
	}
}

// TestMySQLMarkQuizCompletedSetsFlag guards the room-list checkmark: once
// set, QuizCompleted reads back true both from SessionDetail and
// ListSessions (the room list's own source, see sessionsListHandler).
func TestMySQLMarkQuizCompletedSetsFlag(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()
	const userID = "mark-quiz-completed-user"
	sessionID := "sess-mark-quiz-completed"
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
	if meta.QuizCompleted {
		t.Fatalf("QuizCompleted = true before MarkQuizCompleted, want false")
	}

	if err := st.MarkQuizCompleted(ctx, userID, sessionID); err != nil {
		t.Fatalf("MarkQuizCompleted() error = %v", err)
	}

	meta, _, err = st.SessionDetail(ctx, userID, sessionID)
	if err != nil {
		t.Fatalf("SessionDetail() error = %v", err)
	}
	if !meta.QuizCompleted {
		t.Fatalf("QuizCompleted = false after MarkQuizCompleted, want true")
	}

	sessions, err := st.ListSessions(ctx, userID)
	if err != nil {
		t.Fatalf("ListSessions() error = %v", err)
	}
	if len(sessions) != 1 || !sessions[0].QuizCompleted {
		t.Fatalf("ListSessions() = %+v, want QuizCompleted true", sessions)
	}
}

// TestMySQLMarkQuizCompletedMissingSessionIsNoop mirrors EndSession's "no
// row to match" behavior.
func TestMySQLMarkQuizCompletedMissingSessionIsNoop(t *testing.T) {
	st := requireStore(t)
	if err := st.MarkQuizCompleted(context.Background(), "alex", "no-such-session"); err != nil {
		t.Fatalf("MarkQuizCompleted() error = %v, want nil (silent no-op)", err)
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
// guards SaveLearnerProfile's explicit interlocutor_style = '' on first
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

// ---- ReserveAssistantTurn / CompleteAssistantTurn / FailJob / JobStatus ---

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

// ---- ReserveCorrectionJob / SaveCorrection / FailJob (correction) --------

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
