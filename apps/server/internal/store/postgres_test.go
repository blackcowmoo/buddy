package store

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"buddy/server/internal/llm"
)

// newTestPostgres starts a throwaway Postgres in a container for the
// duration of one test. A query-level mock wouldn't have caught the SQLite
// busy-timeout bug this project already hit once — real SQL semantics
// (upserts, JSON columns, connection handling) need a real database. If
// Docker isn't reachable (no daemon, sandboxed CI, restricted dev box), this
// skips rather than failing the whole suite.
func newTestPostgres(t *testing.T) *PostgresStore {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	container, err := tcpostgres.Run(ctx, "postgres:17-alpine",
		tcpostgres.WithDatabase("buddy"),
		tcpostgres.WithUsername("buddy"),
		tcpostgres.WithPassword("buddy"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(30*time.Second),
		),
	)
	if err != nil {
		t.Skipf("postgres testcontainer unavailable (no/unreachable Docker?): %v", err)
	}
	t.Cleanup(func() { _ = container.Terminate(context.Background()) })

	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}

	st, err := NewPostgres(dsn)
	if err != nil {
		t.Fatalf("NewPostgres() error = %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func TestPostgresLoadUnknownUserReturnsZeroProfile(t *testing.T) {
	st := newTestPostgres(t)
	got, err := st.Load(context.Background(), "nobody")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got.Summary != "" || len(got.Recent) != 0 {
		t.Fatalf("expected zero Profile, got %+v", got)
	}
}

func TestPostgresSaveThenLoadRoundTrips(t *testing.T) {
	st := newTestPostgres(t)
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

func TestPostgresSaveTwiceUpserts(t *testing.T) {
	st := newTestPostgres(t)
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
	if err := st.db.QueryRowContext(ctx, `SELECT count(*) FROM profiles WHERE user_id = $1`, "alex").Scan(&n); err != nil {
		t.Fatalf("count query: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected exactly 1 row after upsert, got %d", n)
	}
}

func TestPostgresUsersAreIsolated(t *testing.T) {
	st := newTestPostgres(t)
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
