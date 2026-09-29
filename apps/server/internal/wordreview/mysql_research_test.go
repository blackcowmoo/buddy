package wordreview

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func preparedResearch(t *testing.T) (*MySQLStore, Word, ResearchSuggestion) {
	t.Helper()
	st := requireStore(t)
	ctx := context.Background()
	user := t.Name()
	t.Cleanup(func() {
		if _, err := st.rw.ExecContext(ctx, `DELETE FROM `+table+` WHERE user_id=?`, user); err != nil {
			t.Error(err)
		}
	})
	w, err := st.SaveOriginal(ctx, user, "recapitalize", "재자본화하다", "They recapitalize the road.", "recapitalized")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.rw.ExecContext(ctx, `UPDATE `+table+` SET status=?, verify_reason='wrong object', stage=3,
		review_count=7, correct_streak=2, last_reviewed_at=1700000000,
		review_question_version=2, review_prompt='They ___ the road.', review_answers='["recapitalize"]'
		WHERE id=?`, StatusRejected, w.ID); err != nil {
		t.Fatal(err)
	}
	w, err = st.StartResearch(ctx, user, w.ID)
	if err != nil {
		t.Fatal(err)
	}
	return st, w, ResearchSuggestion{Word: w.Word, Meaning: w.Meaning, Example: "Investors agreed to recapitalize the bank.", Verified: true}
}

func TestResearchSelectionReplacesSameMeaningExampleAndPreservesProgress(t *testing.T) {
	st, before, choice := preparedResearch(t)
	ctx := context.Background()
	done, err := st.FinishResearch(ctx, before, []ResearchSuggestion{choice})
	if err != nil {
		t.Fatal(err)
	}
	// The request cannot certify its own candidate; trust persisted verification.
	choice.Verified = false
	got, err := st.SelectResearch(ctx, before.UserID, before.ID, done.ResearchRevision, choice)
	if err != nil {
		t.Fatal(err)
	}
	want := before
	want.Example, want.Status, want.VerifyReason = choice.Example, StatusVerified, ""
	want.ResearchStatus, want.ResearchResults = ResearchNone, nil
	want.ReviewQuestion = Question{}
	want.MeaningVersion, want.MeaningTargetVersion, want.MeaningStatus = CurrentMeaningVersion, CurrentMeaningVersion, MeaningConfirmed
	want.MeaningRevision++
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("selected=%+v, want=%+v", got, want)
	}
	stored, err := st.Get(ctx, before.UserID, before.ID)
	if err != nil || !reflect.DeepEqual(stored, want) {
		t.Fatalf("persisted=%+v, want=%+v err=%v", stored, want, err)
	}
	late, err := st.FinishResearch(ctx, before, []ResearchSuggestion{{Word: "stale"}})
	if err != nil || !reflect.DeepEqual(late, want) {
		t.Fatalf("late worker changed selection: %+v err=%v", late, err)
	}
	if _, err := st.SelectResearch(ctx, before.UserID, before.ID, done.ResearchRevision, choice); !errors.Is(err, ErrResearchConflict) {
		t.Fatalf("stale selection error=%v", err)
	}
	question := Question{Version: CurrentQuestionVersion, Prompt: "They ___ the road.", Answers: []string{"recapitalize"}}
	if got, written, err := st.SaveQuestion(ctx, before.UserID, before.ID, question, before); err != nil || written || QuestionReady(got) {
		t.Fatalf("old example's question escaped: %+v written=%v err=%v", got, written, err)
	}
	question.Prompt = "Investors agreed to ___ the bank."
	if got, written, err := st.SaveQuestion(ctx, before.UserID, before.ID, question, stored); err != nil || !written || !QuestionReady(got) {
		t.Fatalf("replacement question was not saved: %+v written=%v err=%v", got, written, err)
	}
}

func TestResearchRevisionsProtectNewAttemptsAndTerminalFailure(t *testing.T) {
	st, before, choice := preparedResearch(t)
	ctx := context.Background()
	duplicate, err := st.StartResearch(ctx, before.UserID, before.ID)
	if err != nil || !reflect.DeepEqual(duplicate, before) {
		t.Fatalf("duplicate request restarted pending work: %+v err=%v", duplicate, err)
	}
	failed, err := st.FinishResearch(ctx, before, nil)
	want := before
	want.ResearchStatus = ResearchFailed
	if err != nil || !reflect.DeepEqual(failed, want) {
		t.Fatalf("failure changed the card: %+v err=%v", failed, err)
	}
	retry, err := st.StartResearch(ctx, before.UserID, before.ID)
	if err != nil || retry.ResearchRevision != before.ResearchRevision+1 {
		t.Fatalf("new attempt=%+v err=%v", retry, err)
	}
	for _, results := range [][]ResearchSuggestion{nil, {choice}} {
		got, err := st.FinishResearch(ctx, before, results)
		if err != nil || !reflect.DeepEqual(got, retry) {
			t.Fatalf("old completion changed new attempt: %+v err=%v", got, err)
		}
	}
	done, err := st.FinishResearch(ctx, retry, []ResearchSuggestion{choice})
	if err != nil || done.ResearchStatus != ResearchDone {
		t.Fatalf("new attempt failed: %+v err=%v", done, err)
	}
	if _, err := st.SelectResearch(ctx, before.UserID, before.ID, before.ResearchRevision, choice); !errors.Is(err, ErrResearchConflict) {
		t.Fatalf("old browser selection error=%v", err)
	}
}

func TestResearchSelectionRejectsUntrustedCandidatesWithoutChangingRows(t *testing.T) {
	for _, scenario := range []string{"legacy candidate", "forged example", "pending verification", "meaning cleanup", "duplicate meaning", "wrong owner", "deleted"} {
		t.Run(scenario, func(t *testing.T) {
			st, before, choice := preparedResearch(t)
			ctx := context.Background()
			if scenario == "legacy candidate" {
				choice.Verified = false
			}
			if scenario == "duplicate meaning" {
				choice.Meaning = "자본을 다시 투입하다"
				if _, err := st.Save(ctx, before.UserID, choice.Word, choice.Meaning, "An existing card."); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := st.FinishResearch(ctx, before, []ResearchSuggestion{choice}); err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "forged example":
				choice.Example = "We recapitalize infrastructure."
			case "pending verification":
				if _, err := st.rw.ExecContext(ctx, `UPDATE `+table+` SET status=? WHERE id=?`, StatusPending, before.ID); err != nil {
					t.Fatal(err)
				}
			case "meaning cleanup":
				if _, err := st.rw.ExecContext(ctx, `UPDATE `+table+` SET meaning_status=? WHERE id=?`, MeaningPending, before.ID); err != nil {
					t.Fatal(err)
				}
			case "deleted":
				if err := st.Delete(ctx, before.UserID, before.ID); err != nil {
					t.Fatal(err)
				}
			}
			unchanged, err := st.List(ctx, before.UserID)
			if err != nil {
				t.Fatal(err)
			}
			user := before.UserID
			if scenario == "wrong owner" {
				user = "another user"
			}
			choice.Verified = true
			got, err := st.SelectResearch(ctx, user, before.ID, before.ResearchRevision, choice)
			if scenario == "wrong owner" || scenario == "deleted" {
				if err != nil || got.ID != "" {
					t.Fatalf("missing result=%+v err=%v", got, err)
				}
			} else if !errors.Is(err, ErrResearchConflict) {
				t.Fatalf("unsafe selection succeeded: %+v err=%v", got, err)
			}
			after, err := st.List(ctx, before.UserID)
			if err != nil || !reflect.DeepEqual(after, unchanged) {
				t.Fatalf("failed selection changed rows: before=%+v after=%+v err=%v", unchanged, after, err)
			}
		})
	}
}
