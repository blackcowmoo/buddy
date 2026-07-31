package wordreview

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	tcmysql "github.com/testcontainers/testcontainers-go/modules/mysql"
	"github.com/testcontainers/testcontainers-go/wait"

	"buddy/server/internal/store"
)

// sharedStore backs every container test in this file, started once in
// TestMain — same rationale as internal/store's own mysql_test.go: real SQL
// semantics (the UNIQUE-key upsert in Save, the due-query WHERE clause) need
// a real database, but a fresh container per test would needlessly multiply
// CI time since every test below scopes its own writes by a unique user ID.
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

	container, err := tcmysql.Run(ctx, "mysql:8.0",
		tcmysql.WithDatabase("buddy"),
		tcmysql.WithUsername("buddy"),
		tcmysql.WithPassword("buddy"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("port: 3306  MySQL Community Server").
				WithStartupTimeout(120*time.Second),
		),
	)
	if err != nil {
		// No/unreachable Docker (sandboxed CI, restricted dev box): record why
		// so container tests skip themselves instead of failing the suite, but
		// still run the pure-function tests in wordreview_test.go.
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

	// wordreview shares internal/store's MySQLStore pools in production (see
	// cmd/server/main.go's buildWordReviewStore) — go through the same
	// constructor here so the test exercises the real connection-sharing path.
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

func TestSaveThenListRoundTrips(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()

	saved, err := st.Save(ctx, "alex-list", "ecstatic", "매우 행복한", "She was ecstatic.")
	if err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if saved.Word != "ecstatic" || saved.Stage != 0 || saved.Status != StatusPending {
		t.Fatalf("Save() = %+v, want fresh stage-0 Pending word", saved)
	}
	wantDue := time.Now().Add(stageIntervals[0])
	if saved.NextReviewAt.Before(wantDue.Add(-time.Minute)) || saved.NextReviewAt.After(wantDue.Add(time.Minute)) {
		t.Errorf("NextReviewAt = %v, want ~%v", saved.NextReviewAt, wantDue)
	}

	list, err := st.List(ctx, "alex-list")
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(list) != 1 || list[0].ID != saved.ID {
		t.Fatalf("List() = %+v, want exactly saved %+v", list, saved)
	}
}

// TestSaveIsIdempotentForSameWordAndMeaning guards the core "학습하기"
// behavior: a learner re-searching and re-choosing to study the exact same
// word+meaning must not reset progress already made on it — see
// wordreview's package doc.
func TestSaveIsIdempotentForSameWordAndMeaning(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()

	first, err := st.Save(ctx, "alex-idempotent", "elated", "신이 난", "He was elated.")
	if err != nil {
		t.Fatalf("Save() #1 error = %v", err)
	}
	if _, err := st.MarkVerified(ctx, "alex-idempotent", first.ID, time.Now()); err != nil {
		t.Fatalf("MarkVerified() error = %v", err)
	}
	advanced, err := st.Review(ctx, "alex-idempotent", first.ID, true, time.Now())
	if err != nil {
		t.Fatalf("Review() error = %v", err)
	}

	again, err := st.Save(ctx, "alex-idempotent", "elated", "신이 난", "a different example sentence")
	if err != nil {
		t.Fatalf("Save() #2 error = %v", err)
	}
	if again.ID != first.ID {
		t.Fatalf("Save() #2 ID = %q, want the same row %q", again.ID, first.ID)
	}
	if again.Stage != advanced.Stage {
		t.Errorf("Save() #2 Stage = %d, want unchanged %d (re-saving must not reset progress)", again.Stage, advanced.Stage)
	}
	if again.Status != StatusVerified {
		t.Errorf("Save() #2 Status = %q, want unchanged %q (re-saving must not undo verification)", again.Status, StatusVerified)
	}
	if again.Example != "He was elated." {
		t.Errorf("Save() #2 Example = %q, want the original example kept, not overwritten", again.Example)
	}
}

// TestSaveTracksSameWordDifferentMeaningsIndependently is the direct
// regression test for the multi-meaning requirement: "bank" (riverbank) and
// "bank" (financial) must be two separate rows with independent schedules,
// not deduped into one.
func TestSaveTracksSameWordDifferentMeaningsIndependently(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()

	riverbank, err := st.Save(ctx, "alex-multimeaning", "bank", "강둑", "They sat on the bank.")
	if err != nil {
		t.Fatalf("Save() riverbank error = %v", err)
	}
	financial, err := st.Save(ctx, "alex-multimeaning", "bank", "은행", "I went to the bank.")
	if err != nil {
		t.Fatalf("Save() financial error = %v", err)
	}
	if riverbank.ID == financial.ID {
		t.Fatalf("Save() gave the same row (%q) for two different meanings of \"bank\"", riverbank.ID)
	}

	// Verify and advance only the riverbank sense — the financial sense must
	// stay untouched, proving the two rows really are independent, not just
	// differently-ID'd views of shared state.
	if _, err := st.MarkVerified(ctx, "alex-multimeaning", riverbank.ID, time.Now()); err != nil {
		t.Fatalf("MarkVerified() error = %v", err)
	}
	if _, err := st.Review(ctx, "alex-multimeaning", riverbank.ID, true, time.Now()); err != nil {
		t.Fatalf("Review() error = %v", err)
	}

	list, err := st.List(ctx, "alex-multimeaning")
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("List() = %+v, want two independent rows", list)
	}
	for _, w := range list {
		if w.ID == financial.ID && (w.Stage != 0 || w.Status != StatusPending) {
			t.Errorf("financial sense = %+v, want untouched at stage 0/Pending", w)
		}
		if w.ID == riverbank.ID && (w.Stage != 1 || w.Status != StatusVerified) {
			t.Errorf("riverbank sense = %+v, want stage 1/Verified after review", w)
		}
	}
}

func TestGetReturnsOneWordByID(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()

	saved, err := st.Save(ctx, "alex-get", "wry", "비꼬는", "a wry smile")
	if err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	got, err := st.Get(ctx, "alex-get", saved.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.ID != saved.ID || got.Word != "wry" {
		t.Fatalf("Get() = %+v, want %+v", got, saved)
	}
}

func TestGetIsNoopForUnknownIDOrWrongUser(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()

	if got, err := st.Get(ctx, "alex-get-missing", "does-not-exist"); err != nil || got.ID != "" {
		t.Fatalf("Get(unknown) = %+v, err=%v, want zero Word, nil error", got, err)
	}

	saved, err := st.Save(ctx, "owner-get", "sly", "교활한", "a sly grin")
	if err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if got, err := st.Get(ctx, "someone-else-get", saved.ID); err != nil || got.ID != "" {
		t.Fatalf("Get(wrong user) = %+v, err=%v, want zero Word (no cross-user leak)", got, err)
	}
}

func TestMarkVerifiedTransitionsStatusAndStartsReviewClock(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()

	saved, err := st.Save(ctx, "alex-verify", "keen", "열망하는", "keen to learn")
	if err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	before := time.Now()
	updated, err := st.MarkVerified(ctx, "alex-verify", saved.ID, before)
	if err != nil {
		t.Fatalf("MarkVerified() error = %v", err)
	}
	if updated.Status != StatusVerified {
		t.Errorf("Status = %q, want %q", updated.Status, StatusVerified)
	}
	want := before.Add(stageIntervals[0])
	if updated.NextReviewAt.Before(want.Add(-time.Minute)) || updated.NextReviewAt.After(want.Add(time.Minute)) {
		t.Errorf("NextReviewAt = %v, want ~%v (clock starts at verification time)", updated.NextReviewAt, want)
	}
}

func TestMarkVerifiedIsNoopForUnknownID(t *testing.T) {
	st := requireStore(t)
	got, err := st.MarkVerified(context.Background(), "alex-verify-missing", "does-not-exist", time.Now())
	if err != nil {
		t.Fatalf("MarkVerified() error = %v, want nil (no-op)", err)
	}
	if got.ID != "" {
		t.Fatalf("MarkVerified() = %+v, want zero Word for an unknown id", got)
	}
}

func TestMarkRejectedTransitionsStatusAndRecordsReason(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()

	saved, err := st.Save(ctx, "alex-reject", "xyzzy", "존재하지 않는 단어", "xyzzy the door.")
	if err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	updated, err := st.MarkRejected(ctx, "alex-reject", saved.ID, "\"xyzzy\" is not a real English word")
	if err != nil {
		t.Fatalf("MarkRejected() error = %v", err)
	}
	if updated.Status != StatusRejected {
		t.Errorf("Status = %q, want %q", updated.Status, StatusRejected)
	}
	if updated.VerifyReason != "\"xyzzy\" is not a real English word" {
		t.Errorf("VerifyReason = %q, want the rejection reason preserved", updated.VerifyReason)
	}

	// A rejected word is never silently deleted — it must still show up in
	// List so the learner can see why and remove it themselves.
	list, err := st.List(ctx, "alex-reject")
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(list) != 1 || list[0].Status != StatusRejected {
		t.Fatalf("List() = %+v, want the rejected word still present", list)
	}
}

func TestReviewAdvancesScheduleAndIsReflectedInList(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()

	saved, err := st.Save(ctx, "alex-review", "furious", "화가 난", "She was furious.")
	if err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	updated, err := st.Review(ctx, "alex-review", saved.ID, true, time.Now())
	if err != nil {
		t.Fatalf("Review() error = %v", err)
	}
	if updated.Stage != 1 {
		t.Errorf("Stage = %d, want 1 after one correct answer", updated.Stage)
	}
	if updated.ReviewCount != 1 {
		t.Errorf("ReviewCount = %d, want 1", updated.ReviewCount)
	}

	list, err := st.List(ctx, "alex-review")
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(list) != 1 || list[0].Stage != 1 || list[0].ReviewCount != 1 {
		t.Fatalf("List() = %+v, want the reviewed word's updated schedule", list)
	}
}

func TestReviewIsNoopForUnknownID(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()

	got, err := st.Review(ctx, "alex-review-missing", "does-not-exist", true, time.Now())
	if err != nil {
		t.Fatalf("Review() error = %v, want nil (no-op)", err)
	}
	if got.ID != "" {
		t.Fatalf("Review() = %+v, want zero Word for an unknown id", got)
	}
}

func TestReviewDoesNotAffectOtherUsersWord(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()

	owner, err := st.Save(ctx, "owner-review", "gloomy", "우울한", "It was a gloomy day.")
	if err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	got, err := st.Review(ctx, "someone-else-review", owner.ID, true, time.Now())
	if err != nil {
		t.Fatalf("Review() error = %v", err)
	}
	if got.ID != "" {
		t.Fatalf("Review() by a different user = %+v, want zero Word (no cross-user leak)", got)
	}

	list, err := st.List(ctx, "owner-review")
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(list) != 1 || list[0].Stage != 0 {
		t.Fatalf("owner's word = %+v, want untouched at stage 0", list)
	}
}

func TestDueCountOnlyCountsVerifiedWordsPastDue(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()
	now := time.Now()

	overdue, err := st.Save(ctx, "alex-due", "wistful", "아쉬워하는", "a wistful smile")
	if err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	notYet, err := st.Save(ctx, "alex-due", "melancholy", "우울한", "a melancholy tune")
	if err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	stillPending, err := st.Save(ctx, "alex-due", "forlorn", "쓸쓸한", "a forlorn hope")
	if err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	// overdue and notYet are both verified; overdue is additionally
	// backdated into the past via Review to simulate it coming due, while
	// notYet is left at its fresh ~1-day-out schedule. stillPending is
	// verified never — it must never count, however far in the past its
	// nextReviewAt happens to be (Save's own initial scheduling), since it
	// hasn't passed the model-consensus check yet.
	if _, err := st.MarkVerified(ctx, "alex-due", overdue.ID, now); err != nil {
		t.Fatalf("MarkVerified(overdue) error = %v", err)
	}
	if _, err := st.Review(ctx, "alex-due", overdue.ID, false, now.Add(-48*time.Hour)); err != nil {
		t.Fatalf("Review() error = %v", err)
	}
	if _, err := st.MarkVerified(ctx, "alex-due", notYet.ID, now); err != nil {
		t.Fatalf("MarkVerified(notYet) error = %v", err)
	}
	_ = stillPending

	count, err := st.DueCount(ctx, "alex-due", now)
	if err != nil {
		t.Fatalf("DueCount() error = %v", err)
	}
	if count != 1 {
		t.Fatalf("DueCount() = %d, want 1 (only the backdated, verified word)", count)
	}
}

func TestDeleteRemovesOnlyTheGivenWord(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()

	keep, err := st.Save(ctx, "alex-delete", "content", "만족하는", "I feel content.")
	if err != nil {
		t.Fatalf("Save() #1 error = %v", err)
	}
	remove, err := st.Save(ctx, "alex-delete", "jaded", "지친", "He seemed jaded.")
	if err != nil {
		t.Fatalf("Save() #2 error = %v", err)
	}

	if err := st.Delete(ctx, "alex-delete", remove.ID); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}

	list, err := st.List(ctx, "alex-delete")
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(list) != 1 || list[0].ID != keep.ID {
		t.Fatalf("List() after delete = %+v, want only %+v left", list, keep)
	}
}

func TestDeleteIsNoopForUnknownID(t *testing.T) {
	st := requireStore(t)
	if err := st.Delete(context.Background(), "alex-delete-missing", "does-not-exist"); err != nil {
		t.Fatalf("Delete() error = %v, want nil (no-op)", err)
	}
}
