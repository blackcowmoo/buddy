package store

import (
	"context"
	"errors"
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

// TestMySQLSaveGeneratedTitleIsWriteOnce guards the reason
// SaveGeneratedTitle gates on title_generated instead of unconditionally
// overwriting: internal/transport's trigger fires once per WS *connection*
// (turn 1), not once per session, so a reconnect calling this a second time
// must not flap an already-set title back and forth.
func TestMySQLSaveGeneratedTitleIsWriteOnce(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()
	sessionID := "sess-generated-title-once"
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
	if meta.Title != "First Title" {
		t.Fatalf("Title = %q, want it pinned to the first generated title", meta.Title)
	}
}

func TestMySQLSaveGeneratedTitleNoopWhenSessionMissing(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()
	if err := st.SaveGeneratedTitle(ctx, "alex", "sess-missing-for-title", "Some Title"); err != nil {
		t.Fatalf("SaveGeneratedTitle() error = %v, want nil (no-op)", err)
	}
	if _, _, err := st.SessionDetail(ctx, "alex", "sess-missing-for-title"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("SessionDetail() error = %v, want ErrNotFound (no row should have been created)", err)
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
