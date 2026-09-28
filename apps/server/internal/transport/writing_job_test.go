package transport

import (
	"context"
	"errors"
	"strings"
	"testing"

	"buddy/server/internal/asyncjob"
	"buddy/server/internal/checkpoint"
	"buddy/server/internal/checkpoint/checkpointtest"
	"buddy/server/internal/llm"
	"buddy/server/internal/pipeline"
	"buddy/server/internal/workguard"
	"buddy/server/internal/writing"
)

type resumedWritingStore struct {
	writing.Store
	deletedOwner
	prompt    writing.Prompt
	previous  []writing.Prompt
	listCalls int
	failCalls int
}

func (s *resumedWritingStore) Get(context.Context, string, string) (writing.Prompt, error) {
	return s.prompt, nil
}

func (s *resumedWritingStore) List(context.Context, string) ([]writing.Prompt, error) {
	s.listCalls++
	return s.previous, nil
}

func (s *resumedWritingStore) Complete(_ context.Context, _, korean string) error {
	s.prompt.Korean = korean
	s.prompt.Status = writing.StatusDone
	return nil
}

func (s *resumedWritingStore) Fail(context.Context, string) error {
	s.failCalls++
	s.prompt.Status = writing.StatusFailed
	return nil
}

func TestWritingResumeKeepsProfileAndPreviousPrompts(t *testing.T) {
	st := &resumedWritingStore{prompt: writing.Prompt{ID: "draw-1", Status: writing.StatusPending}, previous: []writing.Prompt{{Korean: "이전 문장"}}}
	var saved checkpointtest.Memory
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	profileText, profileCalls := "original profile", 0
	profile := func(context.Context, string) (string, error) {
		profileCalls++
		return profileText, nil
	}
	var draftInputs, refinementInputs []string
	draft := nuanceLLM{
		messages: func(msgs []llm.Message) { draftInputs = append(draftInputs, msgs[1].Content) },
		complete: func(context.Context) (string, error) { return `{"korean":"새 문장"}`, nil },
	}
	refinement := nuanceLLM{
		messages: func(msgs []llm.Message) { refinementInputs = append(refinementInputs, msgs[1].Content) },
		complete: func(ctx context.Context) (string, error) {
			if len(refinementInputs) == 1 {
				cancel()
				return "", ctx.Err()
			}
			return `{"korean":"완성 문장"}`, nil
		},
	}
	newHandler := func() asyncjob.Handler {
		pipe := &pipeline.Pipeline{LLM: draft, ChatModel: "chat", Analysis: []pipeline.Candidate{{LLM: refinement, Model: "analysis"}}}
		return WritingJobHandler(pipe, st, profile)
	}
	job := asyncjob.Job{Payload: mustPayload(writingJobPayload{PromptID: "draw-1", UserID: "user"})}
	if err := newHandler()(checkpoint.Bind(ctx, &saved), job); !errors.Is(err, context.Canceled) {
		t.Fatalf("interrupted attempt error = %v", err)
	}
	profileText = "updated profile"
	st.previous = append(st.previous, writing.Prompt{Korean: "다른 새 문장"})
	if err := newHandler()(checkpoint.Bind(context.Background(), &saved), job); err != nil {
		t.Fatal(err)
	}
	if len(draftInputs) != 1 || len(refinementInputs) != 2 || refinementInputs[0] != refinementInputs[1] || profileCalls != 1 || st.listCalls != 1 {
		t.Fatalf("resumed draw changed: drafts=%q refinements=%q profile calls=%d list calls=%d", draftInputs, refinementInputs, profileCalls, st.listCalls)
	}
	if st.prompt.Korean != "완성 문장" || st.prompt.Status != writing.StatusDone || st.failCalls != 0 {
		t.Fatalf("resumed prompt = %+v, failure calls=%d", st.prompt, st.failCalls)
	}

	// A distinct draw snapshots the learner's current data instead of sharing
	// the previous job's frozen input or its generated sentence.
	st.prompt = writing.Prompt{ID: "draw-2", Status: writing.StatusPending}
	job.Payload = mustPayload(writingJobPayload{PromptID: "draw-2", UserID: "user"})
	var nextSaved checkpointtest.Memory
	if err := newHandler()(checkpoint.Bind(context.Background(), &nextSaved), job); err != nil {
		t.Fatal(err)
	}
	if len(draftInputs) != 2 || !strings.Contains(draftInputs[1], "updated profile") || !strings.Contains(draftInputs[1], "다른 새 문장") || profileCalls != 2 || st.listCalls != 2 {
		t.Fatalf("new draw reused old inputs: drafts=%q profile calls=%d list calls=%d", draftInputs, profileCalls, st.listCalls)
	}
}

func TestWritingResumeRejectsDeletedOwnerBeforeProfileRead(t *testing.T) {
	st := &resumedWritingStore{prompt: writing.Prompt{ID: "draw", Status: writing.StatusPending}}
	st.deleted.Store(true)
	var saved checkpointtest.Memory
	profile := func(context.Context, string) (string, error) {
		t.Fatal("read profile for deleted writing prompt")
		return "", nil
	}
	err := RunWritingPromptInline(checkpoint.Bind(context.Background(), &saved), nil, st, profile, "draw", "user")
	if !errors.Is(err, workguard.ErrDeleted) || st.failCalls != 0 {
		t.Fatalf("deleted prompt error=%v fail calls=%d", err, st.failCalls)
	}
}
