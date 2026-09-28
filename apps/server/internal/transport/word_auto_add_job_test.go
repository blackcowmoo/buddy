package transport

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"buddy/server/internal/asyncjob"
	"buddy/server/internal/checkpoint"
	"buddy/server/internal/checkpoint/checkpointtest"
	"buddy/server/internal/llm"
	"buddy/server/internal/pipeline"
	"buddy/server/internal/protocol"
	"buddy/server/internal/store"
	"buddy/server/internal/wordreview"
)

const fakeAutoAddSuggestionJSON = `{"suggestions":[{"word":"resilient","meaning":"회복력이 있는","example":"She stayed resilient."},{"word":"savory","meaning":"짭짤한","example":"a savory dish"}]}`

// TestRunWordAutoAddSavesSuggestionsAndCompletesJob guards the primary flow:
// a JobStatusPending run saves every valid suggestion (each starting
// StatusPending, same as a manually picked word), lands JobStatusDone with
// the count of what it actually added, and lets verification finish
// asynchronously afterward.
func TestRunWordAutoAddSavesSuggestionsAndCompletesJob(t *testing.T) {
	st := newFakeStore()
	if err := st.StartWordAutoAdd(context.Background(), "alex"); err != nil {
		t.Fatalf("StartWordAutoAdd() error = %v", err)
	}
	words := newFakeWordReviewStore()
	verificationRelease := make(chan struct{})
	var releaseOnce sync.Once
	releaseVerification := func() { releaseOnce.Do(func() { close(verificationRelease) }) }
	t.Cleanup(releaseVerification)
	pipe := &pipeline.Pipeline{LLM: fakeLLM{completeFn: func(msgs []llm.Message) (string, error) {
		switch {
		case strings.Contains(msgs[0].Content, "strict fact-checker"):
			<-verificationRelease
			return `{"valid":true,"reason":""}`, nil
		case strings.Contains(msgs[0].Content, "fill-in-the-blank recall question"):
			return `{"prompt":"She stayed ___.","answers":["resilient"]}`, nil
		default:
			return fakeAutoAddSuggestionJSON, nil
		}
	}}, ChatModel: "m"}

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

	releaseVerification()
	waitForCondition(t, 2*time.Second, func() bool {
		saved, err := words.List(context.Background(), "alex")
		if err != nil || len(saved) != 2 {
			return false
		}
		for _, w := range saved {
			if w.Status != wordreview.StatusVerified || !wordreview.QuestionReady(w) {
				return false
			}
		}
		return true
	})
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

func TestRunWordAutoAddSkipsOversizedSuggestions(t *testing.T) {
	st := newFakeStore()
	ctx := context.Background()
	if err := st.StartWordAutoAdd(ctx, "alex"); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(map[string]any{"suggestions": []protocol.WordSuggestion{
		{Word: strings.Repeat("a", 256)},
		{Word: "meaning", Meaning: strings.Repeat("뜻", 2001)},
		{Word: "example", Example: strings.Repeat("例", 2001)},
	}})
	if err != nil {
		t.Fatal(err)
	}
	pipe := &pipeline.Pipeline{LLM: fakeLLM{completeFn: func([]llm.Message) (string, error) {
		return string(raw), nil
	}}, ChatModel: "m"}
	words := newFakeWordReviewStore()
	if err := RunWordAutoAddInline(ctx, pipe, words, st, nil, "alex"); err != nil {
		t.Fatal(err)
	}
	status, count, err := st.GetWordAutoAddStatus(ctx, "alex")
	if err != nil || status != store.JobStatusDone || count != 0 {
		t.Fatalf("status=%q count=%d err=%v, want done with no added words", status, count, err)
	}
	saved, err := words.List(ctx, "alex")
	if err != nil || len(saved) != 0 {
		t.Fatalf("saved=%+v err=%v, want no saved words", saved, err)
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

type resumedAutoAddWords struct {
	*fakeWordReviewStore
	cancelAfterSave context.CancelFunc
	saveCalls       []string
	listCalls       int
}

func (s *resumedAutoAddWords) List(ctx context.Context, userID string) ([]wordreview.Word, error) {
	s.listCalls++
	return s.fakeWordReviewStore.List(ctx, userID)
}

func (s *resumedAutoAddWords) Save(_ context.Context, userID, word, meaning, example string) (wordreview.Word, error) {
	s.saveCalls = append(s.saveCalls, word)
	s.mu.Lock()
	w, ok := s.words[word]
	if !ok {
		// Verification is a separate durable job; keep this test focused on
		// resuming the proposal batch without starting detached goroutines.
		w = wordreview.Word{ID: word, UserID: userID, Word: word, Meaning: meaning, Example: example, Status: wordreview.StatusVerified}
		s.words[word] = w
	}
	s.mu.Unlock()
	if s.cancelAfterSave != nil {
		s.cancelAfterSave()
		s.cancelAfterSave = nil
	}
	return w, nil
}

func TestWordAutoAddResumeKeepsGeneratedBatchAfterPartialSave(t *testing.T) {
	st := newFakeStore()
	if err := st.StartWordAutoAdd(context.Background(), "alex"); err != nil {
		t.Fatal(err)
	}
	var saved checkpointtest.Memory
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	words := &resumedAutoAddWords{fakeWordReviewStore: newFakeWordReviewStore(), cancelAfterSave: cancel}
	modelCalls := 0
	pipe := &pipeline.Pipeline{LLM: fakeLLM{completeFn: func([]llm.Message) (string, error) {
		modelCalls++
		return fakeAutoAddSuggestionJSON, nil
	}}, ChatModel: "chat"}
	if err := RunWordAutoAddInline(checkpoint.Bind(ctx, &saved), pipe, words, st, nil, "alex"); !errors.Is(err, context.Canceled) {
		t.Fatalf("interrupted attempt error = %v", err)
	}
	status, _, err := st.GetWordAutoAddStatus(context.Background(), "alex")
	if err != nil || status != store.JobStatusPending || len(words.saveCalls) != 1 {
		t.Fatalf("interrupted status=%q saves=%v error=%v", status, words.saveCalls, err)
	}
	if err := st.SaveLearnerProfile(context.Background(), "alex", "profile changed since the first attempt"); err != nil {
		t.Fatal(err)
	}
	words.words["unrelated"] = wordreview.Word{ID: "unrelated", UserID: "alex", Word: "unrelated", Status: wordreview.StatusVerified}
	resumed := &pipeline.Pipeline{LLM: fakeLLM{completeFn: func([]llm.Message) (string, error) {
		t.Fatal("regenerated suggestions after saving part of the checkpointed batch")
		return "", nil
	}}, ChatModel: "chat"}
	if err := RunWordAutoAddInline(checkpoint.Bind(context.Background(), &saved), resumed, words, st, nil, "alex"); err != nil {
		t.Fatal(err)
	}
	status, count, err := st.GetWordAutoAddStatus(context.Background(), "alex")
	if err != nil || status != store.JobStatusDone || count != 2 || modelCalls != 1 || words.listCalls != 1 {
		t.Fatalf("resumed status=%q count=%d model calls=%d list calls=%d error=%v", status, count, modelCalls, words.listCalls, err)
	}
	if got := strings.Join(words.saveCalls, ","); got != "resilient,resilient,savory" || len(words.words) != 3 {
		t.Fatalf("resumed saves=%q words=%+v", got, words.words)
	}
}

func TestWordAutoAddResumeKeepsInputsWhileRefinementIsInterrupted(t *testing.T) {
	st := newFakeStore()
	if err := st.StartWordAutoAdd(context.Background(), "alex"); err != nil {
		t.Fatal(err)
	}
	var saved checkpointtest.Memory
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	words := &resumedAutoAddWords{fakeWordReviewStore: newFakeWordReviewStore()}
	draftCalls := 0
	draft := nuanceLLM{complete: func(context.Context) (string, error) {
		draftCalls++
		return fakeAutoAddSuggestionJSON, nil
	}}
	var refinementInputs []string
	refinement := nuanceLLM{
		messages: func(msgs []llm.Message) { refinementInputs = append(refinementInputs, msgs[1].Content) },
		complete: func(ctx context.Context) (string, error) {
			if len(refinementInputs) == 1 {
				cancel()
				return "", ctx.Err()
			}
			return fakeAutoAddSuggestionJSON, nil
		},
	}
	newPipeline := func() *pipeline.Pipeline {
		return &pipeline.Pipeline{LLM: draft, ChatModel: "chat", Analysis: []pipeline.Candidate{{LLM: refinement, Model: "analysis"}}}
	}
	if err := RunWordAutoAddInline(checkpoint.Bind(ctx, &saved), newPipeline(), words, st, nil, "alex"); !errors.Is(err, context.Canceled) {
		t.Fatalf("interrupted attempt error = %v", err)
	}
	if err := st.SaveLearnerProfile(context.Background(), "alex", "updated profile"); err != nil {
		t.Fatal(err)
	}
	words.words["unrelated"] = wordreview.Word{ID: "unrelated", UserID: "alex", Word: "unrelated", Status: wordreview.StatusVerified}
	if err := RunWordAutoAddInline(checkpoint.Bind(context.Background(), &saved), newPipeline(), words, st, nil, "alex"); err != nil {
		t.Fatal(err)
	}
	if draftCalls != 1 || words.listCalls != 1 || len(refinementInputs) != 2 || refinementInputs[0] != refinementInputs[1] {
		t.Fatalf("resumed inputs changed: draft calls=%d list calls=%d refinements=%q", draftCalls, words.listCalls, refinementInputs)
	}
	status, count, err := st.GetWordAutoAddStatus(context.Background(), "alex")
	if err != nil || status != store.JobStatusDone || count != 2 {
		t.Fatalf("resumed status=%q count=%d error=%v", status, count, err)
	}
}
