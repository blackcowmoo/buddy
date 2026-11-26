package writing

import (
	"context"
	"os"
	"reflect"
	"testing"
	"time"

	"buddy/server/internal/protocol"
	"buddy/server/internal/store"
	"buddy/server/internal/testdocker"

	"github.com/testcontainers/testcontainers-go"
	tcmysql "github.com/testcontainers/testcontainers-go/modules/mysql"
	"github.com/testcontainers/testcontainers-go/wait"
)

// Shared container per the wordreview/internal-store convention: the SQL
// semantics below (fenced UPDATEs, JSON round-trip, idempotent ALTERs) need
// a real MySQL, and each test scopes its writes by a unique user ID.
var (
	sharedStore    *MySQLStore
	sharedStoreErr error
)

func TestMain(m *testing.M) {
	os.Exit(runContainerTests(m))
}

func runContainerTests(m *testing.M) int {
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	container, err := tcmysql.Run(ctx, "mysql:8.0", testdocker.WithProcessSession(),
		tcmysql.WithDatabase("buddy"),
		tcmysql.WithUsername("buddy"),
		tcmysql.WithPassword("buddy"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("port: 3306  MySQL Community Server").
				WithStartupTimeout(120*time.Second),
		),
	)
	if err != nil {
		// No/unreachable Docker: record why so container tests skip
		// themselves instead of failing the suite.
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
	// Same construction path as production (cmd/server/main.go's
	// buildWritingStore uses internal/store's pools): going through
	// store.NewMySQL also applies the shared migration history, so the test
	// proves the module's additive columns coexist with it.
	profileStore, err := store.NewMySQL(store.MySQLConfig{
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
	defer func() { _ = profileStore.Close() }()
	rw, ro := profileStore.DB()

	st, err := NewMySQL(ctx, rw, ro)
	if err != nil {
		sharedStoreErr = err
		return m.Run()
	}
	// Re-running NewMySQL must be idempotent, including the additive check
	// columns: production starts may re-run it against an existing schema.
	if _, err := NewMySQL(ctx, rw, ro); err != nil {
		sharedStoreErr = err
		return m.Run()
	}
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

func readyPrompt(t *testing.T, st *MySQLStore, userID string) Prompt {
	t.Helper()
	ctx := context.Background()
	p, err := st.Create(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Complete(ctx, p.ID, "오늘은 비가 와요."); err != nil {
		t.Fatal(err)
	}
	p.Korean, p.Status = "오늘은 비가 와요.", StatusDone
	return p
}

func TestStartCheckPersistsAnswerAndResetsPriorResult(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()
	p := readyPrompt(t, st, "start-check")

	if ok, err := st.StartCheck(ctx, "start-check", p.ID, "오늘 비가 와"); err != nil || !ok {
		t.Fatalf("StartCheck ok=%v err=%v", ok, err)
	}
	got, err := st.Get(ctx, "start-check", p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Answer != "오늘 비가 와" || got.CheckStatus != CheckPending || got.Corrected != "" || got.Issues != nil {
		t.Fatalf("after StartCheck = %+v", got)
	}

	// A replaced answer re-parks the row as pending with the old result gone.
	if _, err := st.CompleteCheck(ctx, p.ID, "오늘 비가 와", "오늘은 비가 와요.", []protocol.Issue{{Type: "grammar"}}); err != nil {
		t.Fatal(err)
	}
	if ok, err := st.StartCheck(ctx, "start-check", p.ID, "오늘은 비가 와요"); err != nil || !ok {
		t.Fatalf("re-StartCheck ok=%v err=%v", ok, err)
	}
	if got, err = st.Get(ctx, "start-check", p.ID); err != nil {
		t.Fatal(err)
	}
	if got.Answer != "오늘은 비가 와요" || got.CheckStatus != CheckPending || got.Corrected != "" || got.Issues != nil {
		t.Fatalf("after replaced answer = %+v", got)
	}
}

func TestCompleteCheckRoundTripsIssues(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()
	p := readyPrompt(t, st, "roundtrip")
	if _, err := st.StartCheck(ctx, "roundtrip", p.ID, "오늘 비가 와"); err != nil {
		t.Fatal(err)
	}
	issues := []protocol.Issue{{
		Type: "grammar", Span: "와", Suggestion: "와요",
		Explanation: "Polite sentence ending.", ExplanationTranslation: "해요체 어미가 필요해요.",
	}}
	if ok, err := st.CompleteCheck(ctx, p.ID, "오늘 비가 와", "오늘은 비가 와요.", issues); err != nil || !ok {
		t.Fatalf("CompleteCheck ok=%v err=%v", ok, err)
	}
	got, err := st.Get(ctx, "roundtrip", p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.CheckStatus != CheckDone || got.Corrected != "오늘은 비가 와요." || !reflect.DeepEqual(got.Issues, issues) {
		t.Fatalf("after CompleteCheck = %+v", got)
	}
	// The list read returns the same persisted state as Get.
	items, err := st.List(ctx, "roundtrip")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Answer != "오늘 비가 와" || items[0].CheckStatus != CheckDone || !reflect.DeepEqual(items[0].Issues, issues) {
		t.Fatalf("list = %+v", items)
	}
}

// TestCheckWritesAreFencedToThePendingAnswer pins the guard that keeps a
// stale grade (or a duplicate retry) from overwriting the row a newer
// submission owns.
func TestCheckWritesAreFencedToThePendingAnswer(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()
	p := readyPrompt(t, st, "fence")
	if _, err := st.StartCheck(ctx, "fence", p.ID, "답 하나"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.StartCheck(ctx, "fence", p.ID, "답 둘"); err != nil {
		t.Fatal(err)
	}
	// The in-flight grade for the replaced answer must be dropped.
	if ok, err := st.CompleteCheck(ctx, p.ID, "답 하나", "옛날 결과", nil); err != nil || ok {
		t.Fatalf("stale CompleteCheck ok=%v err=%v, want rejected", ok, err)
	}
	if ok, err := st.FailCheck(ctx, p.ID, "답 하나"); err != nil || ok {
		t.Fatalf("stale FailCheck ok=%v err=%v, want rejected", ok, err)
	}
	if got, err := st.Get(ctx, "fence", p.ID); err != nil || got.CheckStatus != CheckPending {
		t.Fatalf("stale writes changed the row: %+v err=%v", got, err)
	}
	if ok, err := st.CompleteCheck(ctx, p.ID, "답 둘", "오늘은 비가 와요.", nil); err != nil || !ok {
		t.Fatalf("current CompleteCheck ok=%v err=%v", ok, err)
	}
	// Once done, further result writes for the same answer are no-ops: an
	// asyncjob retry cannot resurrect or rewrite a finished check.
	if ok, err := st.CompleteCheck(ctx, p.ID, "답 둘", "경쟁 결과", nil); err != nil || ok {
		t.Fatalf("duplicate CompleteCheck ok=%v err=%v, want rejected", ok, err)
	}
	if ok, err := st.FailCheck(ctx, p.ID, "답 둘"); err != nil || ok {
		t.Fatalf("FailCheck after done ok=%v err=%v, want rejected", ok, err)
	}
	if got, err := st.Get(ctx, "fence", p.ID); err != nil || got.Corrected != "오늘은 비가 와요." {
		t.Fatalf("prompt after duplicate writes = %+v err=%v", got, err)
	}
}

func TestRecheckTransitions(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()

	if ok, err := st.Recheck(ctx, "nobody", "missing"); err != nil || ok {
		t.Fatalf("Recheck unknown ok=%v err=%v, want false", ok, err)
	}

	p := readyPrompt(t, st, "recheck")
	if ok, err := st.Recheck(ctx, "recheck", p.ID); err != nil || ok {
		t.Fatalf("Recheck without an answer ok=%v err=%v, want false", ok, err)
	}

	if _, err := st.StartCheck(ctx, "recheck", p.ID, "오늘 비가 와"); err != nil {
		t.Fatal(err)
	}
	if ok, err := st.CompleteCheck(ctx, p.ID, "오늘 비가 와", "오늘은 비가 와요.", []protocol.Issue{{Type: "grammar"}}); err != nil || !ok {
		t.Fatal(err)
	}

	// Re-checking a finished result clears it and parks the row pending for
	// exactly one new dispatch.
	if ok, err := st.Recheck(ctx, "recheck", p.ID); err != nil || !ok {
		t.Fatalf("Recheck done ok=%v err=%v, want true", ok, err)
	}
	if got, err := st.Get(ctx, "recheck", p.ID); err != nil {
		t.Fatal(err)
	} else if got.CheckStatus != CheckPending || got.Corrected != "" || got.Issues != nil || got.Answer != "오늘 비가 와" {
		t.Fatalf("after Recheck = %+v", got)
	}
	// A second request while pending claims nothing.
	if ok, err := st.Recheck(ctx, "recheck", p.ID); err != nil || ok {
		t.Fatalf("Recheck while pending ok=%v err=%v, want false", ok, err)
	}
	// A failed check is re-runnable too.
	if ok, err := st.FailCheck(ctx, p.ID, "오늘 비가 와"); err != nil || !ok {
		t.Fatal(err)
	}
	if ok, err := st.Recheck(ctx, "recheck", p.ID); err != nil || !ok {
		t.Fatalf("Recheck failed ok=%v err=%v, want true", ok, err)
	}
}
