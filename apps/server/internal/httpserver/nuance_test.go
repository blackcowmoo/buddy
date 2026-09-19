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

	"buddy/server/internal/nuance"
)

type nuanceHTTPStore struct {
	nuance.Store
	lesson nuance.Lesson
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
func (s *nuanceHTTPStore) Delete(_ context.Context, user, id string) error {
	if user == s.lesson.UserID && id == s.lesson.ID {
		s.lesson = nuance.Lesson{}
	}
	return nil
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
