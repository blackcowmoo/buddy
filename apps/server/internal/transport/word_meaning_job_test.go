package transport

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"buddy/server/internal/asyncjob"
	"buddy/server/internal/llm"
	"buddy/server/internal/pipeline"
	"buddy/server/internal/wordreview"
	"buddy/server/internal/workguard"
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

func TestWordMeaningPreflightStartsWithNextContract(t *testing.T) {
	for _, tc := range []struct {
		name      string
		version   int
		response  string
		checkErr  error
		want      string
		wantCalls string
	}{
		{name: "in-flight version unchanged", version: 2, want: "생산 시설", wantCalls: "chat,analysis,judge"},
		{name: "next version skips cleanup", version: 3, response: `{"needsCleanup":false}`, want: "시설", wantCalls: "preflight"},
		{name: "later version skips cleanup", version: 4, response: `{"needsCleanup":false}`, want: "시설", wantCalls: "preflight"},
		{name: "needs cleanup", version: 3, response: `{"needsCleanup":true}`, want: "생산 시설", wantCalls: "preflight,chat,analysis,judge"},
		{name: "missing decision", version: 3, response: `{}`, want: "생산 시설", wantCalls: "preflight,chat,analysis,judge"},
		{name: "null decision", version: 3, response: `{"needsCleanup":null}`, want: "생산 시설", wantCalls: "preflight,chat,analysis,judge"},
		{name: "malformed decision", version: 3, response: `invalid`, want: "생산 시설", wantCalls: "preflight,chat,analysis,judge"},
		{name: "unavailable preflight", version: 3, checkErr: errors.New("chat unavailable"), want: "생산 시설", wantCalls: "preflight,chat,analysis,judge"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls []string
			model := func(stage string) fakeLLM {
				return fakeLLM{completeFn: func(msgs []llm.Message) (string, error) {
					if strings.Contains(msgs[0].Content, "needsCleanup") {
						if stage != "chat" {
							t.Fatal("preflight invoked a slower model")
						}
						calls = append(calls, "preflight")
						return tc.response, tc.checkErr
					}
					calls = append(calls, stage)
					return `{"sameSense":true,"meaning":"생산 시설"}`, nil
				}}
			}
			pipe := &pipeline.Pipeline{LLM: model("chat"), Analysis: []pipeline.Candidate{{LLM: model("analysis")}}, Judge: model("judge"), FeedbackLang: "ko"}
			word := wordreview.Word{Word: "facility", Meaning: "시설", Example: "The steel facility closed.", MeaningTargetVersion: tc.version}
			got, err := wordMeaningForCleanup(context.Background(), pipe, word)
			if err != nil || got != tc.want || strings.Join(calls, ",") != tc.wantCalls {
				t.Fatalf("meaning=%q calls=%v err=%v; want=%q calls=%s", got, calls, err, tc.want, tc.wantCalls)
			}
		})
	}
}

func TestWordMeaningPreflightStopsBeforeFallbackOnDeletionOrCancellation(t *testing.T) {
	for _, reason := range []string{"deleted", "canceled"} {
		t.Run(reason, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			owner := &deletedOwner{}
			ctx = workguard.BindStore(ctx, owner, "alex", "w1")
			calls := 0
			model := fakeLLM{completeFn: func([]llm.Message) (string, error) {
				calls++
				if reason == "deleted" {
					owner.deleted.Store(true)
				} else {
					cancel()
				}
				return `{"needsCleanup":false}`, nil
			}}
			pipe := &pipeline.Pipeline{LLM: model, Analysis: []pipeline.Candidate{{LLM: model}}, Judge: model}
			got, err := wordMeaningForCleanup(ctx, pipe, wordreview.Word{Word: "facility", Meaning: "시설", MeaningTargetVersion: 3})
			wantErr := context.Canceled
			if reason == "deleted" {
				wantErr = workguard.ErrDeleted
			}
			if !errors.Is(err, wantErr) || calls != 1 || got != "" {
				t.Fatalf("meaning=%q calls=%d err=%v; want one call and %v", got, calls, err, wantErr)
			}
		})
	}
}
