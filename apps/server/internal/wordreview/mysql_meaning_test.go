package wordreview

import (
	"context"
	"database/sql"
	"errors"
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
	// A legacy replica can finish a review while this snapshot is being
	// polished. Cleanup must preserve its progress even across a rolling deploy.
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
	want.MeaningStatus = MeaningDone
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
	if !reflect.DeepEqual(retry, after) {
		t.Fatalf("poll retried a failure without a learner choice: %+v", retry)
	}
	kept, err := st.SelectMeaning(ctx, user, old.ID, MeaningChoiceOriginal, after.MeaningRevision)
	if err != nil || kept.MeaningStatus != MeaningConfirmed || kept.Meaning != after.Meaning || kept.MeaningError != "" || kept.ResearchStatus != ResearchConfirmed {
		t.Fatalf("keeping the original after a failed cleanup: %+v err=%v", kept, err)
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

func preparedMeaning(t *testing.T) (*MySQLStore, Word, Word) {
	t.Helper()
	st := requireStore(t)
	ctx := context.Background()
	user := t.Name()
	t.Cleanup(func() {
		if _, err := st.rw.ExecContext(ctx, `DELETE FROM `+table+` WHERE user_id=?`, user); err != nil {
			t.Error(err)
		}
	})
	w, err := st.SaveOriginal(ctx, user, "facility", "맥락상 생산 시설을 의미함", "The facility closed.", "Facility")
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
	if _, _, err := st.SaveQuestion(ctx, user, w.ID, Question{Version: CurrentQuestionVersion, Prompt: "The ___ closed.", Answers: []string{"facility"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Review(ctx, user, w.ID, true, false, now); err != nil {
		t.Fatal(err)
	}
	if err := st.StartMeaningCleanup(ctx, user); err != nil {
		t.Fatal(err)
	}
	pending, err := st.Get(ctx, user, w.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SaveMeaning(ctx, pending, "생산 시설"); err != nil {
		t.Fatal(err)
	}
	done, err := st.Get(ctx, user, w.ID)
	if err != nil {
		t.Fatal(err)
	}
	return st, pending, done
}

func TestSelectCleanedMeaningConfirmsVersionAndPreservesProgress(t *testing.T) {
	st, pending, done := preparedMeaning(t)
	ctx := context.Background()
	// The mutation must return the committed state even with an unavailable replica.
	ro, err := sql.Open("mysql", "buddy:buddy@tcp(127.0.0.1:1)/buddy")
	if err != nil {
		t.Fatal(err)
	}
	if err := ro.Close(); err != nil {
		t.Fatal(err)
	}
	primary := &MySQLStore{rw: st.rw, ro: ro}
	got, err := primary.SelectMeaning(ctx, done.UserID, done.ID, MeaningChoiceCleaned, done.MeaningRevision)
	want := done
	want.MeaningStatus, want.PreviousMeaning = MeaningConfirmed, ""
	want.MeaningVersion = CurrentMeaningVersion
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("selected=%+v want=%+v err=%v", got, want, err)
	}
	if count, err := primary.DueCount(ctx, done.UserID, done.NextReviewAt.Add(time.Hour)); err != nil || count != 1 {
		t.Fatalf("confirmation hidden by replica: count=%d err=%v", count, err)
	}
	if err := st.SaveMeaning(ctx, pending, "stale result"); err != nil {
		t.Fatal(err)
	}
	if err := st.FailMeaning(ctx, pending, "stale error"); err != nil {
		t.Fatal(err)
	}
	// Repeated list refreshes at the same version preserve the choice.
	if err := st.StartMeaningCleanup(ctx, done.UserID); err != nil {
		t.Fatal(err)
	}
	again, err := st.Get(ctx, done.UserID, done.ID)
	if err != nil || !reflect.DeepEqual(again, want) {
		t.Fatalf("confirmed word changed=%+v err=%v", again, err)
	}
	if _, err := st.SelectMeaning(ctx, done.UserID, done.ID, MeaningChoiceOriginal, done.MeaningRevision); !errors.Is(err, ErrMeaningConflict) {
		t.Fatalf("confirmed meaning was restarted: %v", err)
	}
	if got, err := st.SelectMeaning(ctx, done.UserID, done.ID, MeaningChoiceCleaned, done.MeaningRevision); err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("repeated confirmation=%+v err=%v", got, err)
	}
}

func TestSelectOriginalMeaningConfirmsAndRejectsStaleWork(t *testing.T) {
	st, oldWorker, done := preparedMeaning(t)
	ctx := context.Background()
	kept, err := st.SelectMeaning(ctx, done.UserID, done.ID, MeaningChoiceOriginal, done.MeaningRevision)
	want := done
	want.Meaning, want.PreviousMeaning = oldWorker.Meaning, ""
	want.MeaningStatus = MeaningConfirmed
	want.MeaningVersion = CurrentMeaningVersion
	want.ResearchStatus = ResearchConfirmed
	want.MeaningRevision++
	if err != nil || !reflect.DeepEqual(kept, want) {
		t.Fatalf("kept=%+v want=%+v err=%v", kept, want, err)
	}
	if err := st.SaveMeaning(ctx, oldWorker, "stale result"); err != nil {
		t.Fatal(err)
	}
	if err := st.FailMeaning(ctx, oldWorker, "stale error"); err != nil {
		t.Fatal(err)
	}
	for _, choice := range []MeaningChoice{MeaningChoiceCleaned, MeaningChoiceOriginal} {
		if _, err := st.SelectMeaning(ctx, done.UserID, done.ID, choice, done.MeaningRevision); !errors.Is(err, ErrMeaningConflict) {
			t.Fatalf("stale selection %s accepted: %v", choice, err)
		}
	}
	// The choice is final for this contract version: no list refresh or
	// worker poll may queue another cleanup for the kept gloss.
	if err := st.StartMeaningCleanup(ctx, done.UserID); err != nil {
		t.Fatal(err)
	}
	pending, err := st.PendingMeanings(ctx, done.UserID)
	if err != nil || len(pending) != 0 {
		t.Fatalf("kept original was requeued: %+v err=%v", pending, err)
	}
}

func TestMeaningSelectionRejectsWrongOwnerMissingAndConflictingRestoration(t *testing.T) {
	st, _, done := preparedMeaning(t)
	ctx := context.Background()
	for _, key := range [][2]string{{"other-user", done.ID}, {done.UserID, "missing-id"}} {
		got, err := st.SelectMeaning(ctx, key[0], key[1], MeaningChoiceCleaned, done.MeaningRevision)
		if err != nil || got.ID != "" {
			t.Fatalf("unexpected word: %+v err=%v", got, err)
		}
	}
	if _, err := st.Save(ctx, done.UserID, done.Word, done.PreviousMeaning, done.Example); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SelectMeaning(ctx, done.UserID, done.ID, MeaningChoiceOriginal, done.MeaningRevision); !errors.Is(err, ErrMeaningConflict) {
		t.Fatalf("duplicate original not rejected: %v", err)
	}
	got, err := st.Get(ctx, done.UserID, done.ID)
	if err != nil || !reflect.DeepEqual(got, done) {
		t.Fatalf("conflict changed word: %+v err=%v", got, err)
	}
}

func TestMeaningSelectionBlocksReviewUntilConfirmed(t *testing.T) {
	for _, status := range []string{MeaningPending, MeaningDone, MeaningFailed, MeaningConfirmed} {
		t.Run(status, func(t *testing.T) {
			st, _, done := preparedMeaning(t)
			ctx := context.Background()
			if status == MeaningConfirmed {
				if _, err := st.SelectMeaning(ctx, done.UserID, done.ID, MeaningChoiceCleaned, done.MeaningRevision); err != nil {
					t.Fatal(err)
				}
			} else if _, err := st.rw.ExecContext(ctx, `UPDATE `+table+` SET meaning_status=? WHERE id=?`, status, done.ID); err != nil {
				t.Fatal(err)
			}
			now := done.NextReviewAt.Add(time.Hour)
			count, err := st.DueCount(ctx, done.UserID, now)
			want := 0
			if status == MeaningConfirmed {
				want = 1
			}
			if err != nil || count != want {
				t.Fatalf("due=%d want=%d err=%v", count, want, err)
			}
			_, err = st.ReviewVersioned(ctx, done.UserID, done.ID, CurrentQuestionVersion, true, false, now)
			if status == MeaningConfirmed && err != nil || status != MeaningConfirmed && !errors.Is(err, ErrQuestionVersion) {
				t.Fatalf("review state %s: %v", status, err)
			}
		})
	}
}

func TestFailedMeaningKeepsOriginalAsFinalChoice(t *testing.T) {
	st, pending := pendingMeaningAtVersion(t, CurrentMeaningVersion, "")
	ctx := context.Background()
	if err := st.FailMeaning(ctx, pending, "불확실한 뜻"); err != nil {
		t.Fatal(err)
	}
	failed, err := st.Get(ctx, pending.UserID, pending.ID)
	if err != nil || failed.MeaningStatus != MeaningFailed {
		t.Fatalf("failed=%+v err=%v", failed, err)
	}
	// A failed cleanup never invents a gloss: the learner's current meaning
	// stays in place, which is exactly what keeping the original confirms.
	if failed.Meaning != pending.Meaning || failed.MeaningError != "불확실한 뜻" {
		t.Fatalf("failed cleanup changed the baseline: %+v", failed)
	}
	if _, err := st.SelectMeaning(ctx, pending.UserID, pending.ID, MeaningChoiceCleaned, failed.MeaningRevision); !errors.Is(err, ErrMeaningConflict) {
		t.Fatalf("failed result confirmed: %v", err)
	}
	kept, err := st.SelectMeaning(ctx, pending.UserID, pending.ID, MeaningChoiceOriginal, failed.MeaningRevision)
	want := failed
	want.MeaningStatus = MeaningConfirmed
	want.MeaningVersion = CurrentMeaningVersion
	want.MeaningError = ""
	want.ResearchStatus, want.ResearchResults = ResearchConfirmed, nil
	want.MeaningRevision++
	if err != nil || !reflect.DeepEqual(kept, want) {
		t.Fatalf("kept=%+v want=%+v err=%v", kept, want, err)
	}
	if due, err := st.DueCount(ctx, pending.UserID, pending.NextReviewAt.Add(24*time.Hour)); err != nil || due != 1 {
		t.Fatalf("kept failure not study-ready: due=%d err=%v", due, err)
	}
	if err := st.StartMeaningCleanup(ctx, pending.UserID); err != nil {
		t.Fatal(err)
	}
	if again, err := st.Get(ctx, pending.UserID, pending.ID); err != nil || !reflect.DeepEqual(again, want) {
		t.Fatalf("kept failure reopened: %+v err=%v", again, err)
	}
}

func TestMeaningVersionRefreshConfirmsUnchangedAutomatically(t *testing.T) {
	for _, newMeaning := range []string{"생산 시설", "제조 시설"} {
		t.Run(newMeaning, func(t *testing.T) {
			st, _, done := preparedMeaning(t)
			ctx := context.Background()
			// Seed a learner-confirmed result from the previous deployed contract.
			if _, err := st.rw.ExecContext(ctx, `UPDATE `+table+` SET meaning_status='confirmed', previous_meaning=NULL,
		meaning_version=?, meaning_target_version=? WHERE id=?`, CurrentMeaningVersion-1, CurrentMeaningVersion-1, done.ID); err != nil {
				t.Fatal(err)
			}
			previous, err := st.Get(ctx, done.UserID, done.ID)
			if err != nil {
				t.Fatal(err)
			}
			if err := st.StartMeaningCleanup(ctx, done.UserID); err != nil {
				t.Fatal(err)
			}
			pending, err := st.Get(ctx, done.UserID, done.ID)
			want := previous
			want.MeaningStatus = MeaningPending
			want.MeaningTargetVersion = CurrentMeaningVersion
			want.MeaningRevision++
			if err != nil || !reflect.DeepEqual(pending, want) {
				t.Fatalf("version refresh=%+v want=%+v err=%v", pending, want, err)
			}
			// Neither a stale confirmation nor an early one may upgrade this version.
			for _, revision := range []int{previous.MeaningRevision, pending.MeaningRevision} {
				if _, err := st.SelectMeaning(ctx, done.UserID, done.ID, MeaningChoiceCleaned, revision); !errors.Is(err, ErrMeaningConflict) {
					t.Fatalf("premature confirmation revision %d: %v", revision, err)
				}
			}
			for range 2 {
				if err := st.StartMeaningCleanup(ctx, done.UserID); err != nil {
					t.Fatal(err)
				}
			}
			queued, err := st.PendingMeanings(ctx, done.UserID)
			if err != nil || len(queued) != 1 || !reflect.DeepEqual(queued[0], pending) {
				t.Fatalf("poll changed the queued revision: %+v err=%v", queued, err)
			}
			if err := st.SaveMeaning(ctx, pending, newMeaning); err != nil {
				t.Fatal(err)
			}
			result, err := st.Get(ctx, done.UserID, done.ID)
			want.Meaning, want.PreviousMeaning, want.MeaningStatus = newMeaning, previous.Meaning, MeaningDone
			autoConfirmed := newMeaning == previous.Meaning
			if autoConfirmed {
				want.PreviousMeaning, want.MeaningStatus, want.MeaningVersion = "", MeaningConfirmed, CurrentMeaningVersion
			}
			if err != nil || !reflect.DeepEqual(result, want) {
				t.Fatalf("completion=%+v want=%+v err=%v", result, want, err)
			}
			if err := st.StartMeaningCleanup(ctx, done.UserID); err != nil {
				t.Fatal(err)
			}
			count, err := st.DueCount(ctx, done.UserID, done.NextReviewAt.Add(time.Hour))
			wantCount := 0
			if autoConfirmed {
				wantCount = 1
			}
			if err != nil || count != wantCount {
				t.Fatalf("refresh due=%d want=%d err=%v", count, wantCount, err)
			}
			if autoConfirmed {
				if _, err := st.SelectMeaning(ctx, done.UserID, done.ID, MeaningChoiceOriginal, result.MeaningRevision); !errors.Is(err, ErrMeaningConflict) {
					t.Fatalf("automatic confirmation reopened: %v", err)
				}
				return
			}
			// Keeping the previous gloss at a contract refresh is final: it
			// confirms the retained gloss at the reviewed version and keeps
			// the schedule, instead of starting another regeneration.
			kept, err := st.SelectMeaning(ctx, done.UserID, done.ID, MeaningChoiceOriginal, result.MeaningRevision)
			want = result
			want.Meaning, want.PreviousMeaning = previous.Meaning, ""
			want.MeaningStatus, want.MeaningVersion = MeaningConfirmed, CurrentMeaningVersion
			want.ResearchStatus = ResearchConfirmed
			want.MeaningRevision++
			if err != nil || !reflect.DeepEqual(kept, want) {
				t.Fatalf("kept changed confirmed state: %+v want=%+v err=%v", kept, want, err)
			}
			if err := st.SaveMeaning(ctx, pending, "stale worker"); err != nil {
				t.Fatal(err)
			}
			if err := st.StartMeaningCleanup(ctx, done.UserID); err != nil {
				t.Fatal(err)
			}
			after, err := st.Get(ctx, done.UserID, done.ID)
			if err != nil || !reflect.DeepEqual(after, want) {
				t.Fatalf("same version restarted after keeping the original: %+v err=%v", after, err)
			}
		})
	}
}

func TestMeaningRefreshSupersedesOlderResultsAndWorkers(t *testing.T) {
	for _, status := range []string{MeaningPending, MeaningDone, MeaningFailed} {
		t.Run(status, func(t *testing.T) {
			st, _, done := preparedMeaning(t)
			ctx := context.Background()
			if _, err := st.rw.ExecContext(ctx, `UPDATE `+table+` SET meaning_status=?, meaning_target_version=? WHERE id=?`, status, CurrentMeaningVersion-1, done.ID); err != nil {
				t.Fatal(err)
			}
			old, err := st.Get(ctx, done.UserID, done.ID)
			if err != nil {
				t.Fatal(err)
			}
			if err := st.StartMeaningCleanup(ctx, done.UserID); err != nil {
				t.Fatal(err)
			}
			current, err := st.Get(ctx, done.UserID, done.ID)
			if err != nil || current.MeaningStatus != MeaningPending || current.MeaningTargetVersion != CurrentMeaningVersion || current.MeaningRevision != old.MeaningRevision+1 || current.MeaningVersion != old.MeaningVersion {
				t.Fatalf("refresh=%+v err=%v", current, err)
			}
			if err := st.SaveMeaning(ctx, old, "stale result"); err != nil {
				t.Fatal(err)
			}
			if err := st.FailMeaning(ctx, old, "stale failure"); err != nil {
				t.Fatal(err)
			}
			if _, err := st.SelectMeaning(ctx, done.UserID, done.ID, MeaningChoiceCleaned, old.MeaningRevision); !errors.Is(err, ErrMeaningConflict) {
				t.Fatalf("old screen upgraded a new version: %v", err)
			}
			if err := st.SaveMeaning(ctx, current, "제조 시설"); err != nil {
				t.Fatal(err)
			}
			result, err := st.Get(ctx, done.UserID, done.ID)
			if err != nil || result.MeaningStatus != MeaningDone || result.MeaningVersion != old.MeaningVersion || result.PreviousMeaning != done.PreviousMeaning {
				t.Fatalf("refresh lost the unconfirmed baseline: %+v err=%v", result, err)
			}
		})
	}
}

func TestMeaningRefreshLeavesNewerDeploymentWorkUntouched(t *testing.T) {
	st, _, done := preparedMeaning(t)
	ctx := context.Background()
	if _, err := st.rw.ExecContext(ctx, `UPDATE `+table+` SET meaning_status='pending', meaning_target_version=? WHERE id=?`, CurrentMeaningVersion+1, done.ID); err != nil {
		t.Fatal(err)
	}
	future, err := st.Get(ctx, done.UserID, done.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.StartMeaningCleanup(ctx, done.UserID); err != nil {
		t.Fatal(err)
	}
	queued, err := st.PendingMeanings(ctx, done.UserID)
	if err != nil || len(queued) != 0 {
		t.Fatalf("older deployment claimed future work: %+v err=%v", queued, err)
	}
	if err := st.SaveMeaning(ctx, future, "older contract"); err != nil {
		t.Fatal(err)
	}
	if err := st.FailMeaning(ctx, future, "older failure"); err != nil {
		t.Fatal(err)
	}
	after, err := st.Get(ctx, done.UserID, done.ID)
	if err != nil || !reflect.DeepEqual(after, future) {
		t.Fatalf("older deployment changed future work: %+v err=%v", after, err)
	}
}

func TestMeaningConfirmationUsesTheResultVersion(t *testing.T) {
	st, _, done := preparedMeaning(t)
	ctx := context.Background()
	// A deployment can happen between seeing a result and confirming it.
	// Confirmation credits the result actually reviewed, never the new binary.
	if _, err := st.rw.ExecContext(ctx, `UPDATE `+table+` SET meaning_target_version=? WHERE id=?`, CurrentMeaningVersion-1, done.ID); err != nil {
		t.Fatal(err)
	}
	confirmed, err := st.SelectMeaning(ctx, done.UserID, done.ID, MeaningChoiceCleaned, done.MeaningRevision)
	if err != nil || confirmed.MeaningVersion != CurrentMeaningVersion-1 {
		t.Fatalf("old result confirmed a newer version: %+v err=%v", confirmed, err)
	}
	if err := st.StartMeaningCleanup(ctx, done.UserID); err != nil {
		t.Fatal(err)
	}
	next, err := st.Get(ctx, done.UserID, done.ID)
	if err != nil || next.MeaningStatus != MeaningPending || next.MeaningVersion != confirmed.MeaningVersion || next.MeaningTargetVersion != CurrentMeaningVersion || next.MeaningRevision != confirmed.MeaningRevision+1 {
		t.Fatalf("new deployment bypassed review: %+v err=%v", next, err)
	}
}

// Exercise a future deployment without advancing the production migration
// version or changing the behavior of the in-flight version-2 rollout.
func pendingMeaningAtVersion(t *testing.T, version int, previous string) (*MySQLStore, Word) {
	t.Helper()
	st, _, done := preparedMeaning(t)
	ctx := context.Background()
	pendingResearch, err := st.StartResearch(ctx, done.UserID, done.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.FinishResearch(ctx, pendingResearch, []ResearchSuggestion{{Word: "facility", Meaning: "설비", Example: done.Example}}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.rw.ExecContext(ctx, `UPDATE `+table+` SET meaning_status='pending', meaning_error='earlier error',
		meaning_version=?, meaning_target_version=?, meaning_revision=meaning_revision+1, previous_meaning=NULLIF(?, '')
		WHERE id=? AND user_id=?`, version-1, version, previous, done.ID, done.UserID); err != nil {
		t.Fatal(err)
	}
	pending, err := st.Get(ctx, done.UserID, done.ID)
	if err != nil {
		t.Fatal(err)
	}
	return st, pending
}

func TestMeaningIdenticalResultConfirmsAtEveryVersion(t *testing.T) {
	for _, tt := range []struct {
		name      string
		version   int
		previous  string
		meaning   string
		confirmed bool
	}{
		{name: "version 2 unchanged confirms", version: 2, meaning: "생산 시설", confirmed: true},
		{name: "version 2 restored learner baseline confirms", version: 2, previous: "맥락상 생산 시설을 의미함", meaning: "맥락상 생산 시설을 의미함", confirmed: true},
		{name: "version 3 unchanged confirms", version: 3, meaning: "생산 시설", confirmed: true},
		{name: "later version unchanged confirms", version: 4, meaning: "생산 시설", confirmed: true},
		{name: "changed meaning needs choice", version: 3, meaning: "제조 시설"},
		{name: "current version changed meaning needs choice", version: 2, meaning: "제조 시설"},
		{name: "whitespace difference needs choice", version: 3, meaning: "생산 시설 "},
		{name: "repeated suggestion needs choice", version: 3, previous: "맥락상 생산 시설을 의미함", meaning: "생산 시설"},
		{name: "current version repeated suggestion needs choice", version: 2, previous: "맥락상 생산 시설을 의미함", meaning: "생산 시설"},
		{name: "restored learner baseline confirms", version: 3, previous: "맥락상 생산 시설을 의미함", meaning: "맥락상 생산 시설을 의미함", confirmed: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			st, before := pendingMeaningAtVersion(t, tt.version, tt.previous)
			ctx := context.Background()
			// A legacy replica can record a review after the worker snapshot.
			want, err := st.Review(ctx, before.UserID, before.ID, true, false, time.Unix(1700003600, 0))
			if err != nil {
				t.Fatal(err)
			}
			if err := st.saveMeaningForVersion(ctx, before, tt.meaning, tt.version); err != nil {
				t.Fatal(err)
			}
			want.Meaning, want.MeaningError = tt.meaning, ""
			want.MeaningStatus = MeaningDone
			if want.PreviousMeaning == "" {
				want.PreviousMeaning = before.Meaning
			}
			if tt.confirmed {
				want.MeaningStatus, want.MeaningVersion = MeaningConfirmed, tt.version
				want.PreviousMeaning = ""
				want.ResearchStatus, want.ResearchResults = ResearchConfirmed, nil
			}
			got, err := st.Get(ctx, before.UserID, before.ID)
			if err != nil || !reflect.DeepEqual(got, want) {
				t.Fatalf("saved=%+v\nwant=%+v err=%v", got, want, err)
			}
			count, err := st.DueCount(ctx, before.UserID, want.NextReviewAt.Add(time.Hour))
			wantCount := 0
			if tt.confirmed {
				wantCount = 1
			}
			if err != nil || count != wantCount {
				t.Fatalf("due=%d want=%d err=%v", count, wantCount, err)
			}
			// Delayed duplicate completions and failures must not reopen the card.
			if err := st.saveMeaningForVersion(ctx, before, "stale result", tt.version); err != nil {
				t.Fatal(err)
			}
			if err := st.failMeaningForVersion(ctx, before, "stale error", tt.version); err != nil {
				t.Fatal(err)
			}
			again, err := st.Get(ctx, before.UserID, before.ID)
			if err != nil || !reflect.DeepEqual(again, want) {
				t.Fatalf("stale worker changed result=%+v err=%v", again, err)
			}
		})
	}
}

func TestMeaningAutomaticConfirmationRejectsStaleSnapshot(t *testing.T) {
	for _, tt := range []struct {
		name       string
		change     string
		fails      bool
		otherOwner bool
		version    int
	}{
		{name: "meaning edited", change: "meaning='다른 뜻'", fails: true, version: 3},
		{name: "word case edited", change: "word='Facility'", fails: true, version: 3},
		{name: "example edited", change: "example='A different facility.'", fails: true, version: 3},
		{name: "baseline edited", change: "previous_meaning='다른 이전 뜻'", fails: true, version: 3},
		{name: "new revision", change: "meaning_revision=meaning_revision+1", version: 3},
		{name: "new target version", change: "meaning_target_version=4", version: 3},
		{name: "already confirmed version", change: "meaning_version=3", version: 3},
		{name: "already completed", change: "meaning_status='done'", version: 3},
		{name: "older binary", version: 2},
		{name: "other owner", otherOwner: true, version: 3},
		{name: "deleted", change: "DELETE", version: 3},
	} {
		t.Run(tt.name, func(t *testing.T) {
			st, before := pendingMeaningAtVersion(t, 3, "")
			ctx := context.Background()
			if tt.change == "DELETE" {
				if err := st.Delete(ctx, before.UserID, before.ID); err != nil {
					t.Fatal(err)
				}
			} else if tt.change != "" {
				if _, err := st.rw.ExecContext(ctx, `UPDATE `+table+` SET `+tt.change+` WHERE id=? AND user_id=?`, before.ID, before.UserID); err != nil {
					t.Fatal(err)
				}
			}
			want, err := st.Get(ctx, before.UserID, before.ID)
			if err != nil {
				t.Fatal(err)
			}
			worker := before
			if tt.otherOwner {
				worker.UserID = "other-user"
			}
			if err := st.saveMeaningForVersion(ctx, worker, before.Meaning, tt.version); err != nil {
				t.Fatal(err)
			}
			if tt.fails {
				want.MeaningStatus = MeaningFailed
				want.MeaningError = "정리 중 단어가 변경되어 기존 뜻을 유지했어요."
			}
			got, err := st.Get(ctx, before.UserID, before.ID)
			if err != nil || !reflect.DeepEqual(got, want) {
				t.Fatalf("stale snapshot result=%+v\nwant=%+v err=%v", got, want, err)
			}
		})
	}
}

func TestMeaningAutomaticConfirmationKeepsConflictingCards(t *testing.T) {
	st, before := pendingMeaningAtVersion(t, 3, "맥락상 생산 시설을 의미함")
	ctx := context.Background()
	duplicate, err := st.Save(ctx, before.UserID, before.Word, before.PreviousMeaning, before.Example)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.saveMeaningForVersion(ctx, before, before.PreviousMeaning, 3); err != nil {
		t.Fatal(err)
	}
	want := before
	want.MeaningStatus = MeaningFailed
	want.MeaningError = "같은 단어와 뜻의 항목이 있어 기존 뜻을 유지했어요."
	got, err := st.Get(ctx, before.UserID, before.ID)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("conflicting confirmation=%+v want=%+v err=%v", got, want, err)
	}
	other, err := st.Get(ctx, before.UserID, duplicate.ID)
	if err != nil || !reflect.DeepEqual(other, duplicate) {
		t.Fatalf("duplicate changed=%+v want=%+v err=%v", other, duplicate, err)
	}
}

func TestSaveGeneratedSkipsCleanupUntilContractBump(t *testing.T) {
	st := requireStore(t)
	ctx := context.Background()
	user := t.Name()
	t.Cleanup(func() {
		if _, err := st.rw.ExecContext(ctx, `DELETE FROM `+table+` WHERE user_id=?`, user); err != nil {
			t.Error(err)
		}
	})
	w, err := st.SaveGenerated(ctx, user, "facility", "생산 시설", "The steel facility closed.", "Facility")
	if err != nil {
		t.Fatal(err)
	}
	if w.MeaningVersion != CurrentMeaningVersion || w.MeaningStatus != MeaningConfirmed || w.ResearchStatus != ResearchConfirmed {
		t.Fatalf("generated save unstamped: %+v", w)
	}
	now := time.Unix(1700000000, 0)
	if _, err := st.MarkVerified(ctx, user, w.ID, now); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.SaveQuestion(ctx, user, w.ID, Question{Version: CurrentQuestionVersion, Prompt: "The steel ___ closed.", Answers: []string{"facility"}}); err != nil {
		t.Fatal(err)
	}
	if err := st.StartMeaningCleanup(ctx, user); err != nil {
		t.Fatal(err)
	}
	pending, err := st.PendingMeanings(ctx, user)
	if err != nil || len(pending) != 0 {
		t.Fatalf("contract-generated gloss queued for cleanup: %+v err=%v", pending, err)
	}
	if due, err := st.DueCount(ctx, user, now.Add(24*time.Hour)); err != nil || due != 1 {
		t.Fatalf("verified generated word not study-ready: due=%d err=%v", due, err)
	}
	dup, err := st.SaveGenerated(ctx, user, "facility", "생산 시설", "The steel facility closed.", "Facility")
	if err != nil || dup.ID != w.ID {
		t.Fatalf("duplicate generated save=%+v err=%v", dup, err)
	}
	legacy, err := st.SaveOriginal(ctx, user, "facility", "맥락상 설비를 의미함", "The steel facility closed.", "Facility")
	if err != nil {
		t.Fatal(err)
	}
	if legacy.MeaningVersion != 0 || legacy.MeaningStatus != "" {
		t.Fatalf("SaveOriginal must stay unversioned for cleanup to assess: %+v", legacy)
	}
	// A prompt-contract change is the only trigger that re-verifies a
	// generated gloss: simulate the version bump and the card is queued.
	if _, err := st.rw.ExecContext(ctx, `UPDATE `+table+` SET meaning_version=? WHERE id=?`, CurrentMeaningVersion-1, w.ID); err != nil {
		t.Fatal(err)
	}
	if err := st.StartMeaningCleanup(ctx, user); err != nil {
		t.Fatal(err)
	}
	pending, err = st.PendingMeanings(ctx, user)
	if err != nil || len(pending) != 1 || pending[0].ID != w.ID {
		t.Fatalf("version bump must re-queue the generated card: %+v err=%v", pending, err)
	}
}
