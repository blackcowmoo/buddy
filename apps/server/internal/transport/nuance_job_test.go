package transport

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"sync"
	"testing"

	"buddy/server/internal/asyncjob"
	"buddy/server/internal/llm"
	"buddy/server/internal/nuance"
	"buddy/server/internal/pipeline"
)

type nuanceJobStore struct {
	nuance.Store
	mu        sync.Mutex
	lesson    nuance.Lesson
	completed chan nuance.Content
	failed    chan struct{}
}

func (s *nuanceJobStore) Get(context.Context, string, string) (nuance.Lesson, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lesson.ID == "" {
		return nuance.Lesson{}, nuance.ErrNotFound
	}
	return s.lesson, nil
}
func (s *nuanceJobStore) List(context.Context, string) ([]nuance.Lesson, error) {
	return []nuance.Lesson{}, nil
}
func (s *nuanceJobStore) SetStatus(_ context.Context, _ string, status string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lesson.Status = status
	s.lesson.Revision++
	if status == nuance.StatusFailed && s.failed != nil {
		s.failed <- struct{}{}
	}
	return nil
}
func (s *nuanceJobStore) Complete(_ context.Context, _ string, c nuance.Content) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lesson.Content = &c
	s.lesson.Status = nuance.StatusDone
	if s.completed != nil {
		s.completed <- c
	}
	return nil
}

type nuanceLLM struct {
	complete func(context.Context) (string, error)
	messages func([]llm.Message)
}

func (m nuanceLLM) Complete(ctx context.Context, _ string, msgs []llm.Message, _ bool) (string, error) {
	if m.messages != nil {
		m.messages(msgs)
	}
	return m.complete(ctx)
}
func (m nuanceLLM) ChatStream(context.Context, string, []llm.Message, func(string)) (string, error) {
	panic("unused")
}
func TestNuanceInlineSurvivesCancellationAndDeduplicatesPolls(t *testing.T) {
	data, err := os.ReadFile("../nuance/testdata/lesson.json")
	if err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	st := &nuanceJobStore{lesson: nuance.Lesson{ID: t.Name(), Status: nuance.StatusPending}, completed: make(chan nuance.Content, 1)}
	pipe := &pipeline.Pipeline{Analysis: []pipeline.Candidate{{LLM: nuanceLLM{complete: func(ctx context.Context) (string, error) {
		close(entered)
		<-release
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return string(data), nil
	}}}}}
	profile := func(ctx context.Context, _ string) (string, error) { return "learner", ctx.Err() }
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := EnqueueNuance(ctx, nil, pipe, st, profile, "user", t.Name()); err != nil {
		t.Fatal(err)
	}
	<-entered
	if err := EnqueueNuance(ctx, nil, pipe, st, profile, "user", t.Name()); err != nil {
		t.Fatal(err)
	}
	once.Do(func() { close(release) })
	c := <-st.completed
	if len(c.Questions) != 4 {
		t.Fatal("lesson not saved")
	}
}
func TestNuanceWorkerRetriesFailedAndProcessingButSkipsDoneOrDeleted(t *testing.T) {
	data, err := os.ReadFile("../nuance/testdata/lesson.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, status := range []string{nuance.StatusPending, nuance.StatusProcessing, nuance.StatusFailed, nuance.StatusDone, "deleted"} {
		t.Run(status, func(t *testing.T) {
			calls := 0
			st := &nuanceJobStore{lesson: nuance.Lesson{ID: "lesson", Status: status}}
			if status == "deleted" {
				st.lesson.ID = ""
			}
			pipe := &pipeline.Pipeline{Analysis: []pipeline.Candidate{{LLM: nuanceLLM{complete: func(context.Context) (string, error) { calls++; return string(data), nil }}}}}
			payload, _ := json.Marshal(nuanceJobPayload{"user", "lesson"})
			err := NuanceJobHandler(pipe, st, func(context.Context, string) (string, error) { return "", nil })(context.Background(), asyncjob.Job{Payload: payload})
			if err != nil {
				t.Fatal(err)
			}
			want := 1
			if status == nuance.StatusDone || status == "deleted" {
				want = 0
			}
			if calls != want {
				t.Fatalf("calls=%d want=%d", calls, want)
			}
		})
	}
}
func TestNuanceWorkerPersistsFailure(t *testing.T) {
	st := &nuanceJobStore{lesson: nuance.Lesson{ID: "lesson", Status: nuance.StatusPending}}
	payload, _ := json.Marshal(nuanceJobPayload{"user", "lesson"})
	err := NuanceJobHandler(nil, st, func(context.Context, string) (string, error) { return "", errors.New("profile unavailable") })(context.Background(), asyncjob.Job{Payload: payload})
	if err == nil || st.lesson.Status != nuance.StatusFailed {
		t.Fatalf("failure lost: %v %+v", err, st.lesson)
	}
}

func TestNuanceRetryChangesCachedModelInputAfterInvalidGeneration(t *testing.T) {
	data, err := os.ReadFile("../nuance/testdata/lesson.json")
	if err != nil {
		t.Fatal(err)
	}
	st := &nuanceJobStore{lesson: nuance.Lesson{ID: "lesson", Status: nuance.StatusPending}}
	inputs := []string{}
	model := nuanceLLM{
		messages: func(msgs []llm.Message) { inputs = append(inputs, msgs[1].Content) },
		complete: func(context.Context) (string, error) {
			if len(inputs) == 1 {
				return `{}`, nil
			}
			return string(data), nil
		},
	}
	pipe := &pipeline.Pipeline{Analysis: []pipeline.Candidate{{LLM: model}}}
	handler := NuanceJobHandler(pipe, st, func(context.Context, string) (string, error) { return "", nil })
	payload := asyncjob.Job{Payload: mustPayload(nuanceJobPayload{"user", "lesson"})}
	if err := handler(context.Background(), payload); err == nil {
		t.Fatal("invalid model output accepted")
	}
	if err := handler(context.Background(), payload); err != nil {
		t.Fatal(err)
	}
	if len(inputs) != 2 || inputs[0] == inputs[1] || st.lesson.Status != nuance.StatusDone {
		t.Fatalf("retry reuses invalid cached generation: %v", inputs)
	}
}
