package nuance

import (
	"context"
	"database/sql"
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
	integrationStore, integrationErr = NewMySQL(context.Background(), db)
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

func TestMySQLAddsMissingAnswerQuestionsWithoutResettingProgress(t *testing.T) {
	st := requireDB(t)
	ctx := context.Background()
	l := pendingLesson(t, st, t.Name())
	c := testContent()
	for i := range c.Questions {
		c.Questions[i].Answer = "cheap"
	}
	state := State{Progress: map[string]Progress{"q0": {Stage: 2, Attempts: 3, Correct: 2}}, Queue: []string{"q2", "q0"}}
	contentJSON, _ := json.Marshal(c)
	stateJSON, _ := json.Marshal(state)
	if _, err := st.db.ExecContext(ctx, `UPDATE buddy_nuance_lessons SET content_json=?,state_json=?,status=?,revision=? WHERE id=?`, string(contentJSON), string(stateJSON), StatusDone, 7, l.ID); err != nil {
		t.Fatal(err)
	}
	question := Question{
		Context: "중립적인 가격 안내", Sentence: "This option is ____.", Translation: "이 선택지는 저렴합니다.",
		Answer: "inexpensive", Explanation: "inexpensive는 중립적이고 cheap은 품질이 낮다는 인상을 줄 수 있어요.",
	}
	if err := st.AddQuestions(ctx, l.UserID, l.ID, []Question{question}); err != nil {
		t.Fatal(err)
	}
	got, err := st.Get(ctx, l.UserID, l.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Content.Questions) != 6 || got.Content.Questions[5].ID != "q5" || len(got.Content.MissingAnswers()) != 0 || got.Revision != 7 {
		t.Fatalf("supplemented lesson = %+v", got)
	}
	beforeState, _ := json.Marshal(state)
	afterState, _ := json.Marshal(got.State)
	if string(beforeState) != string(afterState) {
		t.Fatalf("progress changed: before=%s after=%s", beforeState, afterState)
	}
	if err := st.AddQuestions(ctx, l.UserID, l.ID, []Question{question}); err != nil {
		t.Fatal(err)
	}
	again, err := st.Get(ctx, l.UserID, l.ID)
	if err != nil || again.Revision != got.Revision || len(again.Content.Questions) != 6 {
		t.Fatalf("repeated supplement changed lesson: %+v %v", again, err)
	}
	if err := st.AddQuestions(ctx, "someone-else", l.ID, []Question{question}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ownership error = %v", err)
	}
}

func TestMySQLStartsReviewWithEveryDueQuestionAcrossLessons(t *testing.T) {
	st := requireDB(t)
	ctx := context.Background()
	first := persistedLesson(t, st)
	second := pendingLesson(t, st, first.UserID)
	if err := st.Complete(ctx, second.ID, comparisonContent("affordable", "budget-friendly")); err != nil {
		t.Fatal(err)
	}

	// Ten due contexts exist across two lessons; the whole queue must be
	// drawn at once instead of a five-question batch.
	batch, err := st.StartReview(ctx, first.UserID)
	if err != nil {
		t.Fatal(err)
	}
	if len(batch.Items) != 10 || len(batch.Lessons) != 2 {
		t.Fatalf("batch = %+v, lessons = %d", batch.Items, len(batch.Lessons))
	}
	wantQueues := map[string][]string{}
	for _, item := range batch.Items {
		wantQueues[item.LessonID] = append(wantQueues[item.LessonID], item.QuestionID)
	}
	queued := 0
	for _, lesson := range batch.Lessons {
		want, selected := wantQueues[lesson.ID]
		if selected && fmt.Sprint(lesson.State.Queue) != fmt.Sprint(want) {
			t.Fatalf("lesson %s queue = %v, want %v", lesson.ID, lesson.State.Queue, want)
		}
		queued += len(lesson.State.Queue)
	}
	if queued != 10 {
		t.Fatalf("persisted %d queued questions, want 10", queued)
	}

	firstItem := batch.Items[0]
	var selected Lesson
	for _, lesson := range batch.Lessons {
		if lesson.ID == firstItem.LessonID {
			selected = lesson
			break
		}
	}
	answer := ""
	for _, question := range selected.Content.Questions {
		if question.ID == firstItem.QuestionID {
			answer = question.Answer
		}
	}
	if _, err = st.Act(ctx, first.UserID, selected.ID, Action{Kind: "answer", Revision: selected.Revision, QuestionID: firstItem.QuestionID, Selected: answer}); err != nil {
		t.Fatal(err)
	}
	resumed, err := st.StartReview(ctx, first.UserID)
	if err != nil || len(resumed.Items) == 0 || resumed.Items[0] != firstItem {
		t.Fatalf("saved reveal was not resumed first: items=%+v err=%v", resumed.Items, err)
	}

	// Acknowledging the reveal reschedules that question beyond its next due
	// date, so the next draw holds every other due question but not it.
	var revealed Lesson
	for _, lesson := range resumed.Lessons {
		if lesson.ID == firstItem.LessonID {
			revealed = lesson
			break
		}
	}
	if _, err = st.Act(ctx, first.UserID, revealed.ID, Action{Kind: "next", Revision: revealed.Revision}); err != nil {
		t.Fatal(err)
	}
	again, err := st.StartReview(ctx, first.UserID)
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Items) != 9 {
		t.Fatalf("remaining due questions = %+v, want 9", again.Items)
	}
	for _, item := range again.Items {
		if item == firstItem {
			t.Fatalf("answered question reappeared before its due date: %+v", again.Items)
		}
	}
}

func TestMySQLReviewBatchResumesFromServerWithoutBrowserItems(t *testing.T) {
	st := requireDB(t)
	ctx := context.Background()
	first := persistedLesson(t, st)
	second := pendingLesson(t, st, first.UserID)
	if err := st.Complete(ctx, second.ID, comparisonContent("affordable", "budget-friendly")); err != nil {
		t.Fatal(err)
	}

	empty, err := st.ReviewState(ctx, first.UserID)
	if err != nil || len(empty.Items) != 0 {
		t.Fatalf("before a draw: items=%+v err=%v", empty.Items, err)
	}
	batch, err := st.StartReview(ctx, first.UserID)
	if err != nil {
		t.Fatal(err)
	}
	if len(batch.Items) != 10 {
		t.Fatalf("batch = %+v", batch.Items)
	}
	resumed, err := st.ReviewState(ctx, first.UserID)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(resumed.Items) != fmt.Sprint(batch.Items) {
		t.Fatalf("replayed order = %+v, want %+v", resumed.Items, batch.Items)
	}
	if len(resumed.Lessons) != 2 {
		t.Fatalf("replayed lessons = %d, want 2", len(resumed.Lessons))
	}

	// Acknowledging a missed question moves it to the end of the saved order,
	// so the browser never has to (and cannot) carry or forge that reorder.
	firstItem := batch.Items[0]
	var selected Lesson
	for _, lesson := range batch.Lessons {
		if lesson.ID == firstItem.LessonID {
			selected = lesson
			break
		}
	}
	answer, wrong := "", ""
	for _, question := range selected.Content.Questions {
		if question.ID == firstItem.QuestionID {
			answer = question.Answer
		}
	}
	for _, word := range selected.Content.Words {
		if word.Word != answer {
			wrong = word.Word
		}
	}
	missed, err := st.Act(ctx, first.UserID, selected.ID, Action{Kind: "answer", Revision: selected.Revision, QuestionID: firstItem.QuestionID, Selected: wrong})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = st.Act(ctx, first.UserID, selected.ID, Action{Kind: "next", Revision: missed.Revision}); err != nil {
		t.Fatal(err)
	}
	wantMoved := append(append([]ReviewItem{}, batch.Items[1:]...), firstItem)
	moved, err := st.ReviewState(ctx, first.UserID)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(moved.Items) != fmt.Sprint(wantMoved) {
		t.Fatalf("order after a missed reveal = %+v, want %+v", moved.Items, wantMoved)
	}

	// A deleted lesson drops its items; the rest of the batch keeps its order.
	if err = st.Delete(ctx, first.UserID, firstItem.LessonID); err != nil {
		t.Fatal(err)
	}
	afterDelete, err := st.ReviewState(ctx, first.UserID)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range afterDelete.Items {
		if item.LessonID == firstItem.LessonID {
			t.Fatalf("deleted lesson still in batch: %+v", afterDelete.Items)
		}
	}

	// A fresh draw replaces the saved batch, and another account sees nothing.
	fresh, err := st.StartReview(ctx, first.UserID)
	if err != nil {
		t.Fatal(err)
	}
	restarted, err := st.ReviewState(ctx, first.UserID)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(restarted.Items) != fmt.Sprint(fresh.Items) {
		t.Fatalf("restarted order = %+v, want %+v", restarted.Items, fresh.Items)
	}
	foreign, err := st.ReviewState(ctx, "someone-else")
	if err != nil || len(foreign.Items) != 0 {
		t.Fatalf("other account batch = %+v err=%v", foreign.Items, err)
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

func pendingLesson(t *testing.T, st *MySQLStore, user string) Lesson {
	t.Helper()
	l, err := st.Create(context.Background(), user)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := st.Delete(context.Background(), user, l.ID); err != nil {
			t.Error(err)
		}
	})
	return l
}

func comparisonContent(words ...string) Content {
	c := testContent()
	c.Words = nil
	for _, word := range words {
		w := testContent().Words[0]
		w.Word = word
		c.Words = append(c.Words, w)
	}
	for i := range c.Questions {
		c.Questions[i].Answer = words[i%len(words)]
	}
	return c
}

func TestMySQLRejectsDuplicateComparisonWithoutPublishing(t *testing.T) {
	st := requireDB(t)
	ctx := context.Background()
	first := persistedLesson(t, st)
	next := pendingLesson(t, st, first.UserID)
	duplicate := comparisonContent("INEXPENSIVE", "Cheap")
	duplicate.Questions[0].Sentence = "A completely different sentence is ____."
	if err := st.Complete(ctx, next.ID, duplicate); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("duplicate accepted: %v", err)
	}
	got, err := st.Get(ctx, next.UserID, next.ID)
	if err != nil || got.Content != nil || got.Status != next.Status || got.Revision != next.Revision {
		t.Fatalf("rejected candidate changed lesson: %+v %v", got, err)
	}
	// The same pending lesson can accept a new pair after the rejected transaction.
	if err := st.Complete(ctx, next.ID, comparisonContent("cheap", "affordable")); err != nil {
		t.Fatal(err)
	}
	other := pendingLesson(t, st, t.Name()+"-other-user")
	if err := st.Complete(ctx, other.ID, duplicate); err != nil {
		t.Fatalf("another account was blocked: %v", err)
	}
	// Cascading deletion releases the reservation, with no stale uniqueness entry.
	if err := st.Delete(ctx, first.UserID, first.ID); err != nil {
		t.Fatal(err)
	}
	replacement := pendingLesson(t, st, first.UserID)
	if err := st.Complete(ctx, replacement.ID, duplicate); err != nil {
		t.Fatalf("deleted comparison still reserved: %v", err)
	}
}

func TestMySQLRejectsLegacyComparisonsBeyondRecentPromptHistory(t *testing.T) {
	st := requireDB(t)
	ctx := context.Background()
	var oldest Lesson
	// Seed pre-migration rows without comparison keys, including a duplicate that
	// already existed. Neither history nor practice data should need deduplication.
	for i := 0; i < 102; i++ {
		l := pendingLesson(t, st, t.Name())
		c := testContent()
		if i > 1 {
			c = comparisonContent(fmt.Sprintf("word-%d", i), "other")
		}
		raw, _ := json.Marshal(c)
		if _, err := st.db.ExecContext(ctx, `UPDATE buddy_nuance_lessons SET content_json=?,status=?,created_at=? WHERE id=?`, string(raw), StatusDone, i, l.ID); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			oldest = l
		}
	}
	next := pendingLesson(t, st, t.Name())
	for _, deleteOldest := range []bool{false, true} {
		if deleteOldest {
			if err := st.Delete(ctx, oldest.UserID, oldest.ID); err != nil {
				t.Fatal(err)
			}
		}
		if err := st.Complete(ctx, next.ID, comparisonContent("INEXPENSIVE", "Cheap")); !errors.Is(err, ErrDuplicate) {
			t.Fatalf("legacy duplicate accepted (deleted oldest=%v): %v", deleteOldest, err)
		}
	}
	if err := st.Complete(ctx, next.ID, comparisonContent("curious", "nosy")); err != nil {
		t.Fatalf("legacy history blocked a different comparison: %v", err)
	}
}

func TestMySQLMigratesStoredWordsToLowercase(t *testing.T) {
	st := requireDB(t)
	ctx := context.Background()
	db := st.db

	// Seed rows stored before words were normalized to lowercase: a lesson
	// with capitalized words, answers, and a pending feedback selection; one
	// already-lowercase lesson; and a pending lesson without content.
	mixed := testContent()
	mixed.Words[0].Word = "Cheap"
	mixed.Words[1].Word = "INEXPENSIVE"
	for i := range mixed.Questions {
		if mixed.Questions[i].Answer == "cheap" {
			mixed.Questions[i].Answer = "CHEAP"
		} else {
			mixed.Questions[i].Answer = "Inexpensive"
		}
	}
	feedbackState := State{
		Progress: map[string]Progress{"q0": {Stage: 1, Attempts: 1, NextReviewAt: 123}},
		Queue:    []string{"q0"},
		Feedback: &Feedback{QuestionID: "q0", Selected: "CHEAP", Correct: false, PreviousStage: 1, AnswerRevision: 4},
	}
	mixedContent, _ := json.Marshal(mixed)
	feedbackJSON, _ := json.Marshal(feedbackState)
	// A distinct user so the already-lowercase lesson seeded below does not
	// collide with this lesson's comparison pair.
	legacy := pendingLesson(t, st, t.Name()+"-legacy")
	if _, err := db.ExecContext(ctx, `UPDATE buddy_nuance_lessons SET content_json=?,state_json=?,status=? WHERE id=?`, string(mixedContent), string(feedbackJSON), StatusDone, legacy.ID); err != nil {
		t.Fatal(err)
	}
	lower := persistedLesson(t, st)
	pending := pendingLesson(t, st, t.Name()+"-pending")
	// Snapshot MySQL-normalized JSON so "unchanged" compares like with like.
	lowerContentBefore, lowerStateBefore := rawLessonJSON(t, db, lower.ID)

	// Re-apply the component migration like a version upgrade does.
	if _, err := db.ExecContext(ctx, `DELETE FROM buddy_schema_migrations WHERE component='nuance' AND version=1`); err != nil {
		t.Fatal(err)
	}
	if _, err := NewMySQL(ctx, db); err != nil {
		t.Fatalf("re-applying migrations: %v", err)
	}

	got, err := st.Get(ctx, legacy.UserID, legacy.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Content.Words[0].Word != "cheap" || got.Content.Words[1].Word != "inexpensive" {
		t.Fatalf("migrated words = %+v", got.Content.Words)
	}
	for _, q := range got.Content.Questions {
		if q.Answer != "cheap" && q.Answer != "inexpensive" {
			t.Fatalf("migrated answer = %q", q.Answer)
		}
	}
	if got.Content.Meaning != mixed.Meaning || got.Revision != 0 {
		t.Fatalf("migration changed unrelated lesson data: %+v", got)
	}
	if got.State.Feedback == nil || got.State.Feedback.Selected != "cheap" || got.State.Feedback.Correct {
		t.Fatalf("feedback after migration = %+v", got.State.Feedback)
	}
	if got.State.Progress["q0"].NextReviewAt != 123 || fmt.Sprint(got.State.Queue) != "[q0]" {
		t.Fatalf("progress after migration = %+v", got.State)
	}
	lowerContentAfter, lowerStateAfter := rawLessonJSON(t, db, lower.ID)
	if lowerContentAfter != lowerContentBefore || lowerStateAfter != lowerStateBefore {
		t.Fatalf("already-lowercase lesson was rewritten: %q/%q -> %q/%q", lowerContentBefore, lowerStateBefore, lowerContentAfter, lowerStateAfter)
	}
	untouchedPending, err := st.Get(ctx, pending.UserID, pending.ID)
	if err != nil || untouchedPending.Status != StatusPending || untouchedPending.Content != nil {
		t.Fatalf("pending lesson changed: %+v %v", untouchedPending, err)
	}

	// The recorded marker keeps the one-time step from running again.
	if _, err := db.ExecContext(ctx, `UPDATE buddy_nuance_lessons SET content_json=? WHERE id=?`, string(mixedContent), legacy.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := NewMySQL(ctx, db); err != nil {
		t.Fatalf("reopening store: %v", err)
	}
	reopened, err := st.Get(ctx, legacy.UserID, legacy.ID)
	if err != nil || reopened.Content.Words[0].Word != "Cheap" {
		t.Fatalf("migration ran twice: %+v %v", reopened.Content, err)
	}
}

// rawLessonJSON returns one lesson's stored JSON columns exactly as MySQL
// returns them, so a test can prove an unchanged row stays byte-identical.
func rawLessonJSON(t *testing.T, db *sql.DB, id string) (string, string) {
	t.Helper()
	var content, state []byte
	if err := db.QueryRowContext(context.Background(), `SELECT content_json,state_json FROM buddy_nuance_lessons WHERE id=?`, id).Scan(&content, &state); err != nil {
		t.Fatal(err)
	}
	return string(content), string(state)
}

func TestMySQLConcurrentDrawsSaveComparisonOnce(t *testing.T) {
	st := requireDB(t)
	ctx := context.Background()
	lessons := []Lesson{pendingLesson(t, st, t.Name()), pendingLesson(t, st, t.Name())}
	start := make(chan struct{})
	results := make(chan error, len(lessons))
	var wg sync.WaitGroup
	for i, l := range lessons {
		wg.Go(func() {
			<-start
			c := testContent()
			if i == 1 {
				c = comparisonContent("INEXPENSIVE", "Cheap")
			}
			results <- st.Complete(ctx, l.ID, c)
		})
	}
	close(start)
	wg.Wait()
	close(results)
	success, duplicate := 0, 0
	for err := range results {
		switch {
		case err == nil:
			success++
		case errors.Is(err, ErrDuplicate):
			duplicate++
		default:
			t.Fatal(err)
		}
	}
	if success != 1 || duplicate != 1 {
		t.Fatalf("success=%d duplicate=%d", success, duplicate)
	}
	saved := 0
	for _, l := range lessons {
		got, err := st.Get(ctx, l.UserID, l.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status == StatusDone && got.Content != nil {
			saved++
		} else if got.Status != StatusPending || got.Content != nil {
			t.Fatalf("partial completion: %+v", got)
		}
	}
	if saved != 1 {
		t.Fatalf("saved %d lessons", saved)
	}
}
