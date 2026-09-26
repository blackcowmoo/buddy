package wordreview

import (
	"context"
	"database/sql"
	"reflect"
	"testing"
	"time"
)

func TestMeaningCleanupPreservesProgressAndOriginal(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()
	user := t.Name()
	t.Cleanup(func() {
		if _, err := st.rw.ExecContext(ctx, `DELETE FROM `+table+` WHERE user_id=?`, user); err != nil {
			t.Error(err)
		}
	})
	w, err := st.Save(ctx, user, "facility", "맥락상 생산 시설을 의미함", "The steel facility closed.")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1700000000, 0)
	if _, err := st.MarkVerified(ctx, user, w.ID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ConfirmResearch(ctx, user, w.ID); err != nil {
		t.Fatal(err)
	}
	q := Question{Version: CurrentQuestionVersion, Prompt: "The ___ closed.", Answers: []string{"facility"}}
	if _, _, err := st.SaveQuestion(ctx, user, w.ID, q); err != nil {
		t.Fatal(err)
	}
	if err := st.StartMeaningCleanup(ctx, user); err != nil {
		t.Fatal(err)
	}
	before, err := st.Get(ctx, user, w.ID)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := st.PendingMeanings(ctx, user)
	if err != nil || len(pending) != 1 || pending[0].ID != w.ID {
		t.Fatalf("pending=%+v err=%v", pending, err)
	}
	// A broken read replica must not hide freshly persisted cleanup state.
	ro, err := sql.Open("mysql", "buddy:buddy@tcp(127.0.0.1:1)/buddy")
	if err != nil {
		t.Fatal(err)
	}
	if err := ro.Close(); err != nil {
		t.Fatal(err)
	}
	primaryOnly := &MySQLStore{rw: st.rw, ro: ro}
	listed, err := primaryOnly.List(ctx, user)
	if err != nil || len(listed) != 1 || listed[0].MeaningStatus != "pending" {
		t.Fatalf("primary list=%+v err=%v", listed, err)
	}
	// The learner can review while the LLM is still polishing this snapshot.
	reviewed, err := st.Review(ctx, user, w.ID, true, false, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SaveMeaning(ctx, before, "생산 시설"); err != nil {
		t.Fatal(err)
	}
	after, err := st.Get(ctx, user, w.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := reviewed
	want.Meaning, want.PreviousMeaning = "생산 시설", before.Meaning
	want.MeaningVersion, want.MeaningStatus = CurrentMeaningVersion, "done"
	if !reflect.DeepEqual(after, want) {
		t.Fatalf("after=%+v\nwant=%+v", after, want)
	}
	if err := st.SaveMeaning(ctx, before, "stale overwrite"); err != nil {
		t.Fatal(err)
	}
	if err := st.StartMeaningCleanup(ctx, user); err != nil {
		t.Fatal(err)
	}
	again, _ := st.Get(ctx, user, w.ID)
	if !reflect.DeepEqual(after, again) {
		t.Fatalf("completed entry changed on retry: %+v", again)
	}
	pending, err = st.PendingMeanings(ctx, user)
	if err != nil || len(pending) != 0 {
		t.Fatalf("completed entry still pending: %+v err=%v", pending, err)
	}
}

func TestMeaningCleanupConflictAndUserIsolation(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()
	user := t.Name()
	t.Cleanup(func() {
		if _, err := st.rw.ExecContext(ctx, `DELETE FROM `+table+` WHERE user_id=?`, user); err != nil {
			t.Error(err)
		}
	})
	old, err := st.Save(ctx, user, "bank", "여기서는 은행을 의미함", "I went to the bank.")
	if err != nil {
		t.Fatal(err)
	}
	duplicate, err := st.Save(ctx, user, "bank", "은행", "I went to the bank.")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.MarkVerified(ctx, user, old.ID, time.Unix(1700000000, 0)); err != nil {
		t.Fatal(err)
	}
	if err := st.StartMeaningCleanup(ctx, "another-user"); err != nil {
		t.Fatal(err)
	}
	untouched, _ := st.Get(ctx, user, old.ID)
	if untouched.MeaningStatus != "" {
		t.Fatal("another user's cleanup touched this entry")
	}
	if err := st.StartMeaningCleanup(ctx, user); err != nil {
		t.Fatal(err)
	}
	old, _ = st.Get(ctx, user, old.ID)
	if err := st.SaveMeaning(ctx, old, "은행"); err != nil {
		t.Fatal(err)
	}
	after, _ := st.Get(ctx, user, old.ID)
	if after.Meaning != old.Meaning || after.MeaningStatus != "failed" || after.MeaningError == "" {
		t.Fatalf("conflict discarded data: %+v", after)
	}
	other, _ := st.Get(ctx, user, duplicate.ID)
	if other.MeaningStatus != "" || other.Status != StatusPending {
		t.Fatalf("pending entry touched: %+v", other)
	}
	if err := st.StartMeaningCleanup(ctx, user); err != nil {
		t.Fatal(err)
	}
	retry, _ := st.Get(ctx, user, old.ID)
	if retry.MeaningStatus != "pending" || retry.MeaningError != "" {
		t.Fatalf("failure cannot retry: %+v", retry)
	}
}

func TestMeaningCleanupDoesNotOverwriteChangedOrDeletedEntry(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()
	user := t.Name()
	t.Cleanup(func() {
		if _, err := st.rw.ExecContext(ctx, `DELETE FROM `+table+` WHERE user_id=?`, user); err != nil {
			t.Error(err)
		}
	})
	w, err := st.Save(ctx, user, "close", "문맥상 문을 닫는다는 뜻", "The shop will close.")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.MarkVerified(ctx, user, w.ID, time.Unix(1700000000, 0)); err != nil {
		t.Fatal(err)
	}
	if err := st.StartMeaningCleanup(ctx, user); err != nil {
		t.Fatal(err)
	}
	w, _ = st.Get(ctx, user, w.ID)
	if _, err := st.rw.ExecContext(ctx, `UPDATE `+table+` SET meaning=? WHERE id=?`, "영업을 마치다", w.ID); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveMeaning(ctx, w, "문을 닫다"); err != nil {
		t.Fatal(err)
	}
	after, _ := st.Get(ctx, user, w.ID)
	if after.Meaning != "영업을 마치다" || after.MeaningStatus != "failed" {
		t.Fatalf("stale worker overwrote entry: %+v", after)
	}
	if err := st.Delete(ctx, user, w.ID); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveMeaning(ctx, w, "문을 닫다"); err != nil {
		t.Fatal(err)
	}
	after, _ = st.Get(ctx, user, w.ID)
	if after.ID != "" {
		t.Fatal("deleted entry was recreated")
	}
}
