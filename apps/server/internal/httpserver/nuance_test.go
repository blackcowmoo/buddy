package httpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"buddy/server/internal/llm"
	"buddy/server/internal/nuance"
	"buddy/server/internal/pipeline"
)

type nuanceHTTPStore struct {
	nuance.Store
	lesson       nuance.Lesson
	supplemented chan struct{}
}

func (s *nuanceHTTPStore) List(_ context.Context, user string) ([]nuance.Lesson, error) {
	if user == s.lesson.UserID {
		return []nuance.Lesson{s.lesson}, nil
	}
	return nil, nil
}
func (s *nuanceHTTPStore) Get(_ context.Context, user, id string) (nuance.Lesson, error) {
	if user != s.lesson.UserID || id != s.lesson.ID {
		return nuance.Lesson{}, nuance.ErrNotFound
	}
	return s.lesson, nil
}
func (s *nuanceHTTPStore) Act(ctx context.Context, user, id string, a nuance.Action) (nuance.Lesson, error) {
	l, err := s.Get(ctx, user, id)
	if err != nil {
		return l, err
	}
	if err = l.Apply(a, time.Unix(1700000000, 0)); err != nil {
		return l, err
	}
	s.lesson = l
	return l, nil
}
func (s *nuanceHTTPStore) StartReview(_ context.Context, user string) (nuance.ReviewBatch, error) {
	if user != s.lesson.UserID {
		return nuance.ReviewBatch{}, nuance.ErrNotFound
	}
	return nuance.ReviewBatch{
		Items:   []nuance.ReviewItem{{LessonID: s.lesson.ID, QuestionID: s.lesson.State.Queue[0]}},
		Lessons: []nuance.Lesson{s.lesson},
	}, nil
}
func (s *nuanceHTTPStore) Delete(_ context.Context, user, id string) error {
	if user == s.lesson.UserID && id == s.lesson.ID {
		s.lesson = nuance.Lesson{}
	}
	return nil
}
func (s *nuanceHTTPStore) AddQuestions(_ context.Context, user, id string, questions []nuance.Question) error {
	if user != s.lesson.UserID || id != s.lesson.ID {
		return nuance.ErrNotFound
	}
	if err := s.lesson.Content.ValidateSupplement(questions); err != nil {
		return err
	}
	s.lesson.Content.Questions = append(s.lesson.Content.Questions, questions...)
	if s.supplemented != nil {
		close(s.supplemented)
	}
	return nil
}

type nuanceHTTPModel struct {
	release <-chan struct{}
}

func (m nuanceHTTPModel) Complete(context.Context, string, []llm.Message, bool) (string, error) {
	<-m.release
	return `{"questions":[{"context":"중립적인 가격표","sentence":"This option is ____.","translation":"이 선택지는 저렴합니다.","answer":"inexpensive","explanation":"inexpensive는 중립적이고 cheap은 품질이 낮다는 인상을 더할 수 있어요."}]}`, nil
}
func (nuanceHTTPModel) ChatStream(context.Context, string, []llm.Message, func(string)) (string, error) {
	panic("unused")
}

func TestNuanceRoutesGradeOnServerAndProtectOwnership(t *testing.T) {
	data, err := os.ReadFile("../nuance/testdata/lesson.json")
	if err != nil {
		t.Fatal(err)
	}
	var c nuance.Content
	if err = json.Unmarshal(data, &c); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, user, method, path, body string
		auth                           bool
		code                           int
	}{
		{"list", "alex", "GET", "/api/nuance", "", true, 200},
		{"review", "alex", "POST", "/api/nuance/review", `{}`, true, 200},
		{"detail", "alex", "GET", "/api/nuance/lesson", "", true, 200},
		{"answer", "alex", "POST", "/api/nuance/lesson/practice", `{"kind":"answer","revision":1,"questionId":"q0","selected":"inexpensive","correct":true}`, true, 200},
		{"stale", "alex", "POST", "/api/nuance/lesson/practice", `{"kind":"answer","revision":0,"questionId":"q0","selected":"cheap"}`, true, 409},
		{"invalid", "alex", "POST", "/api/nuance/lesson/practice", `{"kind":"answer","revision":1,"questionId":"q0","selected":"fake"}`, true, 400},
		{"bad json", "alex", "POST", "/api/nuance/lesson/practice", `{`, true, 400},
		{"other detail", "other", "GET", "/api/nuance/lesson", "", true, 404},
		{"other write", "other", "POST", "/api/nuance/lesson/practice", `{"kind":"start"}`, true, 404},
		{"other retry", "other", "POST", "/api/nuance/lesson/retry", `{}`, true, 404},
		{"other delete", "other", "DELETE", "/api/nuance/lesson", "", true, 204},
		{"delete", "alex", "DELETE", "/api/nuance/lesson", "", true, 204},
		{"anonymous", "alex", "GET", "/api/nuance", "", false, 401},
		{"anonymous draw", "alex", "POST", "/api/nuance/draw", "{}", false, 401},
		{"anonymous review", "alex", "POST", "/api/nuance/review", "{}", false, 401},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := &nuanceHTTPStore{lesson: nuance.Lesson{ID: "lesson", UserID: "alex", Status: nuance.StatusDone, Revision: 1, Content: &c, State: nuance.State{Queue: []string{"q0"}, Progress: map[string]nuance.Progress{}}}}
			mux := http.NewServeMux()
			registerNuance(mux, fakeIdentifier{id: tc.user, ok: tc.auth}, st, nil, nil, nil)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body)))
			if rec.Code != tc.code {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			if tc.name == "answer" && st.lesson.State.Feedback.Correct {
				t.Fatal("trusted client verdict")
			}
			if tc.name == "other delete" && st.lesson.ID == "" {
				t.Fatal("deleted another account's lesson")
			}
			if strings.Contains(rec.Body.String(), "userId") {
				t.Fatal("leaked account metadata")
			}
		})
	}
}

func TestNuanceListBackfillsMissingAnswerCoverageAndReportsProcessing(t *testing.T) {
	data, err := os.ReadFile("../nuance/testdata/lesson.json")
	if err != nil {
		t.Fatal(err)
	}
	var c nuance.Content
	if err := json.Unmarshal(data, &c); err != nil {
		t.Fatal(err)
	}
	for i := range c.Questions {
		c.Questions[i].Answer = "cheap"
	}
	release := make(chan struct{})
	st := &nuanceHTTPStore{
		lesson:       nuance.Lesson{ID: "lesson", UserID: "alex", Status: nuance.StatusDone, Content: &c, State: nuance.State{Progress: map[string]nuance.Progress{}}},
		supplemented: make(chan struct{}),
	}
	pipe := &pipeline.Pipeline{Analysis: []pipeline.Candidate{{LLM: nuanceHTTPModel{release: release}}}}
	mux := http.NewServeMux()
	registerNuance(mux, fakeIdentifier{id: "alex", ok: true}, st, pipe, nil, nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/nuance", nil))
	var lessons []nuance.Lesson
	if err := json.Unmarshal(rec.Body.Bytes(), &lessons); err != nil {
		t.Fatal(err)
	}
	if len(lessons) != 1 || lessons[0].Status != nuance.StatusProcessing {
		t.Fatalf("list response = %+v", lessons)
	}
	close(release)
	<-st.supplemented
	if len(st.lesson.Content.MissingAnswers()) != 0 || st.lesson.Status != nuance.StatusDone {
		t.Fatalf("saved lesson = %+v", st.lesson)
	}
}
