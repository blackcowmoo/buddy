package transport

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"buddy/server/internal/asyncjob"
	"buddy/server/internal/pipeline"
	"buddy/server/internal/wordreview"
)

type meaningJobStore struct {
	*fakeWordReviewStore
	*deletedOwner
	saves, failures int
	afterSave       func()
}

func (s *meaningJobStore) StartMeaningCleanup(context.Context, string) error { return nil }
func (s *meaningJobStore) PendingMeanings(ctx context.Context, userID string) ([]wordreview.Word, error) {
	return s.List(ctx, userID)
}
func (s *meaningJobStore) SaveMeaning(_ context.Context, before wordreview.Word, meaning string) error {
	s.saves++
	w := s.words[before.ID]
	w.PreviousMeaning, w.Meaning = w.Meaning, meaning
	w.MeaningStatus = "done"
	s.words[w.ID] = w
	if s.afterSave != nil {
		s.afterSave()
	}
	return nil
}
func (s *meaningJobStore) FailMeaning(_ context.Context, before wordreview.Word, reason string) error {
	s.failures++
	w := s.words[before.ID]
	w.MeaningStatus, w.MeaningError = "failed", reason
	s.words[w.ID] = w
	return nil
}

func TestMeaningCleanupJobResumesAndPreservesStudyState(t *testing.T) {
	w := wordreview.Word{ID: "w1", UserID: "alex", Word: "facility", Meaning: "맥락상 생산 시설을 의미함", Example: "The facility closed.", Status: wordreview.StatusVerified, MeaningStatus: "pending", MeaningTargetVersion: wordreview.CurrentMeaningVersion, Stage: 4, ReviewCount: 12, NextReviewAt: time.Unix(1700000000, 0), ResearchStatus: wordreview.ResearchConfirmed}
	done := w
	done.ID = "done"
	done.MeaningStatus = "done"
	w.MeaningVersion = wordreview.CurrentMeaningVersion - 1
	other := w
	other.ID = "other"
	other.UserID = "sam"
	future := w
	future.ID = "future"
	future.MeaningTargetVersion++
	older := w
	older.ID = "older"
	older.MeaningTargetVersion--
	st := &meaningJobStore{fakeWordReviewStore: newFakeWordReviewStore(w, done, other, future, older), deletedOwner: &deletedOwner{}}
	pipe := &pipeline.Pipeline{LLM: fakeAnalysisLLM{complete: `{"sameSense":true,"meaning":"생산 시설"}`}, FeedbackLang: "ko"}
	payload, _ := json.Marshal(wordResearchJobPayload{UserID: "alex", CleanupMeanings: true})
	handler := WordResearchJobHandler(pipe, st)
	for i := 0; i < 2; i++ {
		if err := handler(context.Background(), asyncjob.Job{Kind: asyncjob.KindWordResearch, Payload: payload}); err != nil {
			t.Fatal(err)
		}
	}
	w.PreviousMeaning, w.Meaning = w.Meaning, "생산 시설"
	w.MeaningStatus = "done"
	if !reflect.DeepEqual(st.words[w.ID], w) || st.saves != 1 || st.failures != 0 {
		t.Fatalf("saved=%+v saves=%d failures=%d", st.words[w.ID], st.saves, st.failures)
	}
	if !reflect.DeepEqual(st.words[other.ID], other) {
		t.Fatal("modified another user")
	}
	for _, skipped := range []wordreview.Word{done, future, older} {
		if !reflect.DeepEqual(st.words[skipped.ID], skipped) {
			t.Fatalf("modified finished work or a different deployment's target: %+v", st.words[skipped.ID])
		}
	}
}

func TestMeaningCleanupUncertaintyKeepsOldMeaning(t *testing.T) {
	w := wordreview.Word{ID: "w1", UserID: "alex", Status: wordreview.StatusVerified, MeaningStatus: "pending", MeaningTargetVersion: wordreview.CurrentMeaningVersion, Word: "bank", Meaning: "불명확한 원문"}
	st := &meaningJobStore{fakeWordReviewStore: newFakeWordReviewStore(w), deletedOwner: &deletedOwner{}}
	pipe := &pipeline.Pipeline{LLM: fakeAnalysisLLM{complete: `{"sameSense":false,"meaning":""}`}}
	if err := RunWordMeaningCleanupInline(context.Background(), pipe, st, "alex"); err != nil {
		t.Fatal(err)
	}
	got := st.words[w.ID]
	if st.saves != 0 || st.failures != 1 || got.Meaning != w.Meaning || got.MeaningStatus != "failed" || got.MeaningError == "" {
		t.Fatalf("got=%+v", got)
	}
}

func TestMeaningCleanupStopsAfterDeletionDuringModelCall(t *testing.T) {
	w := wordreview.Word{ID: "w1", UserID: "alex", Status: wordreview.StatusVerified, MeaningStatus: "pending", MeaningTargetVersion: wordreview.CurrentMeaningVersion, Word: "facility"}
	owner := &deletedOwner{}
	model := &deletingModel{owner: owner}
	st := &meaningJobStore{fakeWordReviewStore: newFakeWordReviewStore(w), deletedOwner: owner}
	pipe := &pipeline.Pipeline{LLM: model, Analysis: []pipeline.Candidate{{LLM: model}}, Judge: model}
	if err := RunWordMeaningCleanupInline(context.Background(), pipe, st, "alex"); err != nil {
		t.Fatal(err)
	}
	if model.calls != 1 || st.saves != 0 || st.failures != 0 {
		t.Fatalf("calls=%d saves=%d failures=%d", model.calls, st.saves, st.failures)
	}
}

func TestMeaningCleanupDrainsRetryRequestedDuringActiveBatch(t *testing.T) {
	w := wordreview.Word{ID: "w1", UserID: "alex", Word: "facility", Meaning: "맥락상 생산 시설을 의미함", Example: "The facility closed.", Status: wordreview.StatusVerified, MeaningStatus: wordreview.MeaningPending, MeaningTargetVersion: wordreview.CurrentMeaningVersion, MeaningRevision: 1}
	st := &meaningJobStore{fakeWordReviewStore: newFakeWordReviewStore(w), deletedOwner: &deletedOwner{}}
	st.afterSave = func() {
		if st.saves == 1 {
			// The selection arrives after the first result is persisted but
			// before the active job has released its dedupe key.
			retry := w
			retry.MeaningRevision++
			st.words[w.ID] = retry
		}
	}
	pipe := &pipeline.Pipeline{LLM: fakeAnalysisLLM{complete: `{"sameSense":true,"meaning":"생산 시설"}`}, FeedbackLang: "ko"}
	if err := RunWordMeaningCleanupInline(context.Background(), pipe, st, w.UserID); err != nil {
		t.Fatal(err)
	}
	got := st.words[w.ID]
	if st.saves != 2 || got.MeaningStatus != wordreview.MeaningDone || got.MeaningRevision != 2 || got.PreviousMeaning != w.Meaning {
		t.Fatalf("retry stranded: saves=%d word=%+v", st.saves, got)
	}
}
