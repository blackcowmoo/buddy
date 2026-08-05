package transport

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"buddy/server/internal/asyncjob"
	"buddy/server/internal/llm"
	"buddy/server/internal/pipeline"
	"buddy/server/internal/store"
	"buddy/server/internal/wordreview"
)

const fakeAutoAddSuggestionJSON = `{"suggestions":[{"word":"resilient","meaning":"회복력이 있는","example":"She stayed resilient."},{"word":"savory","meaning":"짭짤한","example":"a savory dish"}]}`

// TestRunWordAutoAddSavesSuggestionsAndCompletesJob guards the primary flow:
// a JobStatusPending run saves every valid suggestion (each starting
// StatusPending, same as a manually picked word) and lands JobStatusDone
// with the count of what it actually added.
func TestRunWordAutoAddSavesSuggestionsAndCompletesJob(t *testing.T) {
	st := newFakeStore()
	if err := st.StartWordAutoAdd(context.Background(), "alex"); err != nil {
		t.Fatalf("StartWordAutoAdd() error = %v", err)
	}
	words := newFakeWordReviewStore()
	pipe := &pipeline.Pipeline{LLM: fakeLLM{completeFn: func(msgs []llm.Message) (string, error) { return fakeAutoAddSuggestionJSON, nil }}, ChatModel: "m"}

	if err := RunWordAutoAddInline(context.Background(), pipe, words, st, nil, "alex"); err != nil {
		t.Fatalf("RunWordAutoAddInline() error = %v", err)
	}

	status, count, err := st.GetWordAutoAddStatus(context.Background(), "alex")
	if err != nil {
		t.Fatalf("GetWordAutoAddStatus() error = %v", err)
	}
	if status != store.JobStatusDone || count != 2 {
		t.Fatalf("status/count = %q/%d, want %q/2", status, count, store.JobStatusDone)
	}

	saved, err := words.List(context.Background(), "alex")
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(saved) != 2 {
		t.Fatalf("saved words = %+v, want 2", saved)
	}
	for _, w := range saved {
		if w.Status != wordreview.StatusPending {
			t.Errorf("word %q status = %q, want %q — verification runs separately", w.Word, w.Status, wordreview.StatusPending)
		}
	}
}

// TestRunWordAutoAddIsNoopWhenNotPending guards against a stale reap-retry
// (or a racing second call) redoing generation once a run has already
// reached a terminal status.
func TestRunWordAutoAddIsNoopWhenNotPending(t *testing.T) {
	st := newFakeStore() // GetWordAutoAddStatus returns "" — never started
	words := newFakeWordReviewStore()
	calls := 0
	pipe := &pipeline.Pipeline{LLM: fakeLLM{completeFn: func(msgs []llm.Message) (string, error) {
		calls++
		return fakeAutoAddSuggestionJSON, nil
	}}, ChatModel: "m"}

	if err := RunWordAutoAddInline(context.Background(), pipe, words, st, nil, "alex"); err != nil {
		t.Fatalf("RunWordAutoAddInline() error = %v", err)
	}
	if calls != 0 {
		t.Fatalf("expected no LLM call when the job was never marked pending, got %d calls", calls)
	}
}

// TestRunWordAutoAddMarksFailedWhenSuggestFails guards that a SuggestNewWords
// failure both propagates the error (so asyncjob's own claim-shortening
// applies) and records JobStatusFailed for the frontend's poll to observe.
func TestRunWordAutoAddMarksFailedWhenSuggestFails(t *testing.T) {
	st := newFakeStore()
	if err := st.StartWordAutoAdd(context.Background(), "alex"); err != nil {
		t.Fatalf("StartWordAutoAdd() error = %v", err)
	}
	words := newFakeWordReviewStore()
	pipe := &pipeline.Pipeline{LLM: fakeLLM{}, ChatModel: "m"} // always errors

	if err := RunWordAutoAddInline(context.Background(), pipe, words, st, nil, "alex"); err == nil {
		t.Fatal("expected an error when SuggestNewWords fails")
	}

	status, _, err := st.GetWordAutoAddStatus(context.Background(), "alex")
	if err != nil {
		t.Fatalf("GetWordAutoAddStatus() error = %v", err)
	}
	if status != store.JobStatusFailed {
		t.Fatalf("status = %q, want %q", status, store.JobStatusFailed)
	}
}

// TestRunWordAutoAddSkipsSuggestionsFailingValidation guards that an
// individual bad suggestion (empty word) is skipped, not fatal to the whole
// batch — the run still completes with only the valid ones counted.
func TestRunWordAutoAddSkipsSuggestionsFailingValidation(t *testing.T) {
	st := newFakeStore()
	if err := st.StartWordAutoAdd(context.Background(), "alex"); err != nil {
		t.Fatalf("StartWordAutoAdd() error = %v", err)
	}
	words := newFakeWordReviewStore()
	pipe := &pipeline.Pipeline{LLM: fakeLLM{completeFn: func(msgs []llm.Message) (string, error) {
		return `{"suggestions":[{"word":"  ","meaning":"blank","example":"x"},{"word":"savory","meaning":"짭짤한","example":"a savory dish"}]}`, nil
	}}, ChatModel: "m"}

	if err := RunWordAutoAddInline(context.Background(), pipe, words, st, nil, "alex"); err != nil {
		t.Fatalf("RunWordAutoAddInline() error = %v", err)
	}

	_, count, err := st.GetWordAutoAddStatus(context.Background(), "alex")
	if err != nil {
		t.Fatalf("GetWordAutoAddStatus() error = %v", err)
	}
	if count != 1 {
		t.Fatalf("addedCount = %d, want 1 (the blank suggestion skipped)", count)
	}
}

func TestWordAutoAddJobHandlerBadPayload(t *testing.T) {
	st := newFakeStore()
	handler := WordAutoAddJobHandler(&pipeline.Pipeline{}, newFakeWordReviewStore(), st, nil)
	job := asyncjob.Job{Kind: asyncjob.KindWordAutoAdd, Payload: json.RawMessage(`not json`)}
	if err := handler(context.Background(), job); err == nil {
		t.Fatalf("handler(bad payload) error = nil, want an unmarshal error")
	}
}

// TestEnqueueWordAutoAddJobRunsInBackgroundAndPersists mirrors
// TestEnqueueWordVerifyJobRunsInBackgroundAndPersists against real Redis.
func TestEnqueueWordAutoAddJobRunsInBackgroundAndPersists(t *testing.T) {
	rdb := requireReplyRedis(t)
	queue := asyncjob.NewQueue(rdb)
	st := newFakeStore()
	if err := st.StartWordAutoAdd(context.Background(), "alex"); err != nil {
		t.Fatalf("StartWordAutoAdd() error = %v", err)
	}
	words := newFakeWordReviewStore()
	pipe := &pipeline.Pipeline{LLM: fakeLLM{completeFn: func(msgs []llm.Message) (string, error) { return fakeAutoAddSuggestionJSON, nil }}, ChatModel: "m"}

	if err := EnqueueWordAutoAddJob(context.Background(), queue, pipe, words, st, nil, "alex"); err != nil {
		t.Fatalf("EnqueueWordAutoAddJob() error = %v", err)
	}

	waitForCondition(t, 2*time.Second, func() bool {
		status, _, _ := st.GetWordAutoAddStatus(context.Background(), "alex")
		return status == store.JobStatusDone
	})
	saved, err := words.List(context.Background(), "alex")
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(saved) != 2 {
		t.Fatalf("saved words = %+v, want 2", saved)
	}
}
