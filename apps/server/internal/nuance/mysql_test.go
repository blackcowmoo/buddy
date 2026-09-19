package nuance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"buddy/server/internal/testdocker"

	"buddy/server/internal/store"
	"github.com/testcontainers/testcontainers-go"
	tcmysql "github.com/testcontainers/testcontainers-go/modules/mysql"
	"github.com/testcontainers/testcontainers-go/wait"
)

var integrationStore *MySQLStore
var integrationErr error

func TestMain(m *testing.M) { os.Exit(runTests(m)) }
func runTests(m *testing.M) int {
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	c, err := tcmysql.Run(ctx, "mysql:8.0", testdocker.WithProcessSession(), tcmysql.WithDatabase("buddy"), tcmysql.WithUsername("buddy"), tcmysql.WithPassword("buddy"), testcontainers.WithWaitStrategy(wait.ForLog("port: 3306  MySQL Community Server").WithStartupTimeout(120*time.Second)))
	if err != nil {
		integrationErr = err
		return m.Run()
	}
	defer c.Terminate(context.Background())
	if !strings.HasSuffix(c.SessionID(), fmt.Sprintf("-%d", os.Getpid())) {
		integrationErr = fmt.Errorf("container cleanup session is not process-scoped: %s", c.SessionID())
		return m.Run()
	}
	host, err := c.Host(ctx)
	if err != nil {
		integrationErr = err
		return m.Run()
	}
	port, err := c.MappedPort(ctx, "3306/tcp")
	if err != nil {
		integrationErr = err
		return m.Run()
	}
	st, err := store.NewMySQL(store.MySQLConfig{RWHost: host, Port: int(port.Num()), User: "buddy", Password: "buddy", Database: "buddy"})
	if err != nil {
		integrationErr = err
		return m.Run()
	}
	defer st.Close()
	db, _ := st.DB()
	integrationStore = NewMySQL(db)
	return m.Run()
}
func requireDB(t *testing.T) *MySQLStore {
	t.Helper()
	if integrationErr != nil {
		t.Fatalf("MySQL integration setup: %v", integrationErr)
	}
	return integrationStore
}
func persistedLesson(t *testing.T, st *MySQLStore) Lesson {
	t.Helper()
	ctx := context.Background()
	l, err := st.Create(ctx, t.Name())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := st.Delete(ctx, l.UserID, l.ID); err != nil {
			t.Error(err)
		}
	})
	if err = st.Complete(ctx, l.ID, testContent()); err != nil {
		t.Fatal(err)
	}
	l, err = st.Get(ctx, l.UserID, l.ID)
	if err != nil {
		t.Fatal(err)
	}
	return l
}
func TestMySQLPersistsResumeHistoryOwnershipAndDeletion(t *testing.T) {
	st := requireDB(t)
	ctx := context.Background()
	l := persistedLesson(t, st)
	act := func(a Action) {
		t.Helper()
		a.Revision = l.Revision
		var err error
		l, err = st.Act(ctx, l.UserID, l.ID, a)
		if err != nil {
			t.Fatal(err)
		}
	}
	act(Action{Kind: "start"})
	act(Action{Kind: "answer", QuestionID: "q0", Selected: "cheap"})
	restored, err := st.Get(ctx, l.UserID, l.ID)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := json.Marshal(l)
	after, _ := json.Marshal(restored)
	if string(before) != string(after) {
		t.Fatal("reopened state differs")
	}
	if _, err = st.Get(ctx, "someone-else", l.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ownership: %v", err)
	}
	if _, err = st.Act(ctx, "someone-else", l.ID, Action{Kind: "next", Revision: l.Revision}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("write ownership: %v", err)
	}
	act(Action{Kind: "next", Repeat: true})
	var correct, repeat bool
	if err = st.db.QueryRowContext(ctx, `SELECT correct,repeat_review FROM buddy_nuance_attempts WHERE lesson_id=?`, l.ID).Scan(&correct, &repeat); err != nil || !correct || !repeat {
		t.Fatalf("history: %v %v %v", correct, repeat, err)
	}
	if err = st.Delete(ctx, "someone-else", l.ID); err != nil {
		t.Fatal(err)
	}
	items, err := st.List(ctx, l.UserID)
	if err != nil || len(items) != 1 {
		t.Fatalf("list: %v %v", items, err)
	}
	if err = st.Delete(ctx, l.UserID, l.ID); err != nil {
		t.Fatal(err)
	}
	var n int
	if err = st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM buddy_nuance_attempts WHERE lesson_id=?`, l.ID).Scan(&n); err != nil || n != 0 {
		t.Fatalf("cascade: %d %v", n, err)
	}
}
func TestMySQLConcurrentAnswerCommitsOnce(t *testing.T) {
	st := requireDB(t)
	ctx := context.Background()
	l := persistedLesson(t, st)
	l, err := st.Act(ctx, l.UserID, l.ID, Action{Kind: "start", Revision: l.Revision})
	if err != nil {
		t.Fatal(err)
	}
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Go(func() {
			_, err := st.Act(ctx, l.UserID, l.ID, Action{Kind: "answer", QuestionID: "q0", Selected: "cheap", Revision: l.Revision})
			results <- err
		})
	}
	wg.Wait()
	close(results)
	success, conflict := 0, 0
	for err := range results {
		if err == nil {
			success++
		} else if errors.Is(err, ErrConflict) {
			conflict++
		} else {
			t.Fatal(err)
		}
	}
	if success != 1 || conflict != 1 {
		t.Fatalf("success=%d conflict=%d", success, conflict)
	}
	got, err := st.Get(ctx, l.UserID, l.ID)
	if err != nil || got.State.Progress["q0"].Attempts != 1 {
		t.Fatalf("double advanced: %+v %v", got, err)
	}
}
func TestMySQLLateGenerationCannotReplacePracticedContent(t *testing.T) {
	st := requireDB(t)
	ctx := context.Background()
	l := persistedLesson(t, st)
	changed := testContent()
	changed.Meaning = "late response"
	if err := st.Complete(ctx, l.ID, changed); err != nil {
		t.Fatal(err)
	}
	if err := st.SetStatus(ctx, l.ID, StatusFailed); err != nil {
		t.Fatal(err)
	}
	got, err := st.Get(ctx, l.UserID, l.ID)
	if err != nil || got.Content.Meaning != l.Content.Meaning || got.Status != StatusDone || got.Revision != l.Revision {
		t.Fatalf("late write changed lesson: %+v %v", got, err)
	}
}

func TestMySQLGenerationTransitionsAdvanceRevisionForFreshRetry(t *testing.T) {
	st := requireDB(t)
	ctx := context.Background()
	l, err := st.Create(ctx, t.Name())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Delete(ctx, l.UserID, l.ID) })
	for _, status := range []string{StatusProcessing, StatusFailed, StatusPending} {
		if err := st.SetStatus(ctx, l.ID, status); err != nil {
			t.Fatal(err)
		}
		next, err := st.Get(ctx, l.UserID, l.ID)
		if err != nil {
			t.Fatal(err)
		}
		if next.Revision <= l.Revision || next.Status != status {
			t.Fatalf("generation revision not advanced: %+v", next)
		}
		l = next
	}
}
