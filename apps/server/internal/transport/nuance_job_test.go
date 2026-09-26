package transport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"

	"buddy/server/internal/asyncjob"
	"buddy/server/internal/llm"
	"buddy/server/internal/nuance"
	"buddy/server/internal/pipeline"
)

type nuanceJobStore struct {
	nuance.Store
	mu               sync.Mutex
	lesson           nuance.Lesson
	completed        chan nuance.Content
	failed           chan struct{}
	lessons          []nuance.Lesson
	completionErrors []error
	completionCalls  int
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
	return s.lessons, nil
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
	s.completionCalls++
	if s.completionCalls <= len(s.completionErrors) && s.completionErrors[s.completionCalls-1] != nil {
		return s.completionErrors[s.completionCalls-1]
	}
	s.lesson.Content = &c
	s.lesson.Status = nuance.StatusDone
	if s.completed != nil {
		s.completed <- c
	}
	return nil
}
func (s *nuanceJobStore) AddQuestions(_ context.Context, _, _ string, questions []nuance.Question) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lesson.Content == nil {
		return nuance.ErrInvalid
	}
	if err := s.lesson.Content.ValidateSupplement(questions); err != nil {
		return err
	}
	for i := range questions {
		questions[i].ID = fmt.Sprintf("q%d", len(s.lesson.Content.Questions)+i)
	}
	s.lesson.Content.Questions = append(s.lesson.Content.Questions, questions...)
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
	if len(c.Questions) != 5 {
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
			payload, _ := json.Marshal(nuanceJobPayload{UserID: "user", LessonID: "lesson"})
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
	payload, _ := json.Marshal(nuanceJobPayload{UserID: "user", LessonID: "lesson"})
	err := NuanceJobHandler(nil, st, func(context.Context, string) (string, error) { return "", errors.New("profile unavailable") })(context.Background(), asyncjob.Job{Payload: payload})
	if err == nil || st.lesson.Status != nuance.StatusFailed {
		t.Fatalf("failure lost: %v %+v", err, st.lesson)
	}
}

func TestNuanceWorkerSupplementsMissingLegacyAnswers(t *testing.T) {
	data, err := os.ReadFile("../nuance/testdata/lesson.json")
	if err != nil {
		t.Fatal(err)
	}
	var content nuance.Content
	if err := json.Unmarshal(data, &content); err != nil {
		t.Fatal(err)
	}
	for i := range content.Questions {
		content.Questions[i].Answer = "cheap"
	}
	st := &nuanceJobStore{lesson: nuance.Lesson{ID: "lesson", UserID: "user", Status: nuance.StatusDone, Content: &content}}
	model := nuanceLLM{complete: func(context.Context) (string, error) {
		return `{"questions":[{"context":"중립적인 가격표","sentence":"This option is ____.","translation":"이 선택지는 저렴합니다.","answer":"inexpensive","explanation":"inexpensive는 중립적이고 cheap은 품질이 낮다는 인상을 더할 수 있어요."}]}`, nil
	}}
	pipe := &pipeline.Pipeline{Analysis: []pipeline.Candidate{{LLM: model}}}
	job := asyncjob.Job{Payload: mustPayload(nuanceJobPayload{
		UserID: "user", LessonID: "lesson", Operation: nuanceSupplementOperation, RequestID: "supplement-1",
	})}
	if err := NuanceJobHandler(pipe, st, nil)(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	if len(st.lesson.Content.Questions) != 6 || len(st.lesson.Content.MissingAnswers()) != 0 || st.lesson.Status != nuance.StatusDone {
		t.Fatalf("supplemented lesson = %+v", st.lesson)
	}
}

func TestNuanceInvalidGenerationIsTerminalAndExplicitRetryGetsFreshInput(t *testing.T) {
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
	payload := asyncjob.Job{Payload: mustPayload(nuanceJobPayload{UserID: "user", LessonID: "lesson"})}
	if err := handler(context.Background(), payload); err != nil {
		t.Fatalf("terminal model output error must not trigger an automatic job retry: %v", err)
	}
	if st.lesson.Status != nuance.StatusFailed {
		t.Fatalf("invalid generation status = %q, want failed", st.lesson.Status)
	}
	if err := handler(context.Background(), payload); err != nil {
		t.Fatal(err)
	}
	if len(inputs) != 2 || inputs[0] == inputs[1] || st.lesson.Status != nuance.StatusDone {
		t.Fatalf("retry reuses invalid cached generation: %v", inputs)
	}
}

func TestNuanceDuplicateRegeneratesWithRejectedPairAndFreshRequest(t *testing.T) {
	data, err := os.ReadFile("../nuance/testdata/lesson.json")
	if err != nil {
		t.Fatal(err)
	}
	st := &nuanceJobStore{
		lesson:           nuance.Lesson{ID: "lesson", Status: nuance.StatusPending},
		completionErrors: []error{nuance.ErrDuplicate},
	}
	// The database may reject a pair outside the most recent 100 exclusions,
	// or one concurrently saved after List returned.
	for i := 0; i < 101; i++ {
		st.lessons = append(st.lessons, nuance.Lesson{Content: &nuance.Content{
			Words: []nuance.Word{{Word: fmt.Sprintf("word-%d", i)}, {Word: "other"}},
		}})
	}
	var inputs []struct {
		Previous  []string `json:"previous"`
		RequestID string   `json:"requestId"`
	}
	model := nuanceLLM{
		messages: func(msgs []llm.Message) {
			var input struct {
				Previous  []string `json:"previous"`
				RequestID string   `json:"requestId"`
			}
			if err := json.Unmarshal([]byte(msgs[1].Content), &input); err != nil {
				t.Fatal(err)
			}
			inputs = append(inputs, input)
		},
		complete: func(context.Context) (string, error) {
			if len(inputs) == 1 {
				return string(data), nil
			}
			return strings.ReplaceAll(string(data), "cheap", "affordable"), nil
		},
	}
	pipe := &pipeline.Pipeline{Analysis: []pipeline.Candidate{{LLM: model}}}
	job := asyncjob.Job{Payload: mustPayload(nuanceJobPayload{UserID: "user", LessonID: "lesson"})}
	if err := NuanceJobHandler(pipe, st, func(context.Context, string) (string, error) { return "", nil })(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	if len(inputs) != 2 || st.completionCalls != 2 || st.lesson.Status != nuance.StatusDone || st.lesson.Content.Words[0].Word != "affordable" {
		t.Fatalf("duplicate was not replaced: inputs=%+v lesson=%+v", inputs, st.lesson)
	}
	if inputs[0].RequestID == inputs[1].RequestID || !slices.Contains(inputs[1].Previous, "cheap / inexpensive") {
		t.Fatalf("retry did not exclude rejected pair with fresh input: %+v", inputs)
	}
	for _, input := range inputs {
		if len(input.Previous) != 100 || slices.Contains(input.Previous, "word-0 / other") {
			t.Fatalf("incorrect recent exclusions: %+v", input)
		}
	}
}

func TestNuanceDuplicateAttemptsAreBoundedAndOtherErrorsStop(t *testing.T) {
	data, err := os.ReadFile("../nuance/testdata/lesson.json")
	if err != nil {
		t.Fatal(err)
	}
	dbErr := errors.New("database unavailable")
	for _, tc := range []struct {
		name   string
		errors []error
		want   error
		calls  int
	}{
		{"duplicates exhausted", []error{nuance.ErrDuplicate, nuance.ErrDuplicate, nuance.ErrDuplicate}, nuance.ErrDuplicate, 3},
		{"database failure", []error{dbErr}, dbErr, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := &nuanceJobStore{lesson: nuance.Lesson{ID: "lesson", Status: nuance.StatusPending}, completionErrors: tc.errors}
			calls := 0
			model := nuanceLLM{complete: func(context.Context) (string, error) { calls++; return string(data), nil }}
			pipe := &pipeline.Pipeline{Analysis: []pipeline.Candidate{{LLM: model}}}
			job := asyncjob.Job{Payload: mustPayload(nuanceJobPayload{UserID: "user", LessonID: "lesson"})}
			err := NuanceJobHandler(pipe, st, func(context.Context, string) (string, error) { return "", nil })(context.Background(), job)
			if !errors.Is(err, tc.want) || calls != tc.calls || st.lesson.Status != nuance.StatusFailed || st.lesson.Content != nil {
				t.Fatalf("error=%v calls=%d lesson=%+v", err, calls, st.lesson)
			}
		})
	}
}
