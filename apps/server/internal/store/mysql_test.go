package store

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	tcmysql "github.com/testcontainers/testcontainers-go/modules/mysql"
	"github.com/testcontainers/testcontainers-go/wait"

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

// TestMySQLUsesGolangMigrateHistory verifies that startup records the
// baseline in golang-migrate's version/dirty table. A clean version marker is
// what prevents a second replica from re-running DDL after the advisory lock
// is released.
func TestMySQLUsesGolangMigrateHistory(t *testing.T) {
	st := requireStore(t)
	var version int
	var dirty bool
	if err := st.rw.QueryRow(`SELECT version, dirty FROM buddy_migrate_schema_migrations`).Scan(&version, &dirty); err != nil {
		t.Fatalf("golang-migrate history: %v", err)
	}
	if version != 1 || dirty {
		t.Fatalf("golang-migrate history = version %d, dirty %v; want version 1, clean", version, dirty)
	}
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
	// TestMain has already bootstrapped the shared container. Remove only this
	// marker so the rows below model data that existed before this migration.
	if _, err := st.rw.ExecContext(ctx, `DELETE FROM buddy_schema_migrations WHERE component = 'store' AND version = 14`); err != nil {
		t.Fatalf("reset migration marker: %v", err)
	}

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

	// A later startup must not run the data rewrite again after the step has
	// been recorded.
	if _, err := reopened.rw.ExecContext(ctx, `UPDATE `+sessionsTable+` SET study_summary = ?, study_summary_status = 'done' WHERE user_id = ? AND id = ?`, "legacy text after migration", userID, legacySession); err != nil {
		t.Fatalf("seed post-migration value: %v", err)
	}
	again, err := NewMySQL(sharedStoreConfig)
	if err != nil {
		t.Fatalf("second NewMySQL() error = %v", err)
	}
	defer again.Close()
	var summary, status string
	if err := again.rw.QueryRowContext(ctx, `SELECT study_summary, study_summary_status FROM `+sessionsTable+` WHERE user_id = ? AND id = ?`, userID, legacySession).Scan(&summary, &status); err != nil {
		t.Fatalf("read post-migration value: %v", err)
	}
	if summary != "legacy text after migration" || status != JobStatusDone {
		t.Fatalf("post-migration value = (%q, %q), want unchanged data", summary, status)
	}
}
