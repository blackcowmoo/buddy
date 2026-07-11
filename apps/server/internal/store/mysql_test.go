package store

import (
	"context"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	tcmysql "github.com/testcontainers/testcontainers-go/modules/mysql"
	"github.com/testcontainers/testcontainers-go/wait"

	"buddy/server/internal/llm"
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
			if got := hostPort(tt.host, tt.fallbackPort); got != tt.want {
				t.Errorf("hostPort(%q, %d) = %q, want %q", tt.host, tt.fallbackPort, got, tt.want)
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

	st, err := NewMySQL(MySQLConfig{
		RWHost:   host,
		Port:     int(port.Num()),
		User:     "buddy",
		Password: "buddy",
		Database: "buddy",
	})
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

func TestMySQLLoadUnknownUserReturnsZeroProfile(t *testing.T) {
	st := requireStore(t)
	got, err := st.Load(context.Background(), "nobody")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got.Summary != "" || len(got.Recent) != 0 {
		t.Fatalf("expected zero Profile, got %+v", got)
	}
}

func TestMySQLSaveThenLoadRoundTrips(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()
	want := Profile{
		Summary: "likes hiking",
		Recent: []llm.Message{
			{Role: llm.RoleUser, Content: "hi"},
			{Role: llm.RoleAssistant, Content: "hello"},
		},
	}
	if err := st.Save(ctx, "alex", want); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	got, err := st.Load(ctx, "alex")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Load() = %+v, want %+v", got, want)
	}
}

func TestMySQLSaveTwiceUpserts(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()
	if err := st.Save(ctx, "alex", Profile{Summary: "v1"}); err != nil {
		t.Fatalf("Save() #1 error = %v", err)
	}
	if err := st.Save(ctx, "alex", Profile{Summary: "v2"}); err != nil {
		t.Fatalf("Save() #2 error = %v", err)
	}

	got, err := st.Load(ctx, "alex")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got.Summary != "v2" {
		t.Fatalf("Summary = %q, want v2 (upsert should overwrite)", got.Summary)
	}

	var n int
	if err := st.rw.QueryRowContext(ctx, `SELECT count(*) FROM buddy_profiles WHERE user_id = ?`, "alex").Scan(&n); err != nil {
		t.Fatalf("count query: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected exactly 1 row after upsert, got %d", n)
	}
}

func TestMySQLUsersAreIsolated(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()
	if err := st.Save(ctx, "alex", Profile{Summary: "alex's memory"}); err != nil {
		t.Fatalf("Save(alex) error = %v", err)
	}
	if err := st.Save(ctx, "sam", Profile{Summary: "sam's memory"}); err != nil {
		t.Fatalf("Save(sam) error = %v", err)
	}

	a, err := st.Load(ctx, "alex")
	if err != nil {
		t.Fatalf("Load(alex) error = %v", err)
	}
	s, err := st.Load(ctx, "sam")
	if err != nil {
		t.Fatalf("Load(sam) error = %v", err)
	}
	if a.Summary != "alex's memory" || s.Summary != "sam's memory" {
		t.Fatalf("cross-contamination between users: alex=%+v sam=%+v", a, s)
	}
}
