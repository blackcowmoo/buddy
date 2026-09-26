package httpserver

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"buddy/server/internal/pipeline"
	"buddy/server/internal/wordreview"
)

type meaningHTTPStore struct {
	*fakeWordStore
	startedFor string
}

func (s *meaningHTTPStore) PendingMeanings(ctx context.Context, userID string) ([]wordreview.Word, error) {
	return s.List(ctx, userID)
}

func (s *meaningHTTPStore) StartMeaningCleanup(_ context.Context, user string) error {
	s.startedFor = user
	return nil
}
func (s *meaningHTTPStore) SaveMeaning(context.Context, wordreview.Word, string) error { return nil }
func (s *meaningHTTPStore) FailMeaning(context.Context, wordreview.Word, string) error { return nil }

func TestMeaningCleanupHandlerScopesToAuthenticatedUser(t *testing.T) {
	st := &meaningHTTPStore{fakeWordStore: &fakeWordStore{}}
	var scheduledFor string
	schedule := func(_ context.Context, user string) { scheduledFor = user }
	h := wordMeaningCleanupHandler(fakeIdentifier{id: "alex", ok: true}, st, schedule)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/api/words/meanings/cleanup", strings.NewReader(`{"userID":"sam"}`)))
	requireStatus(t, rec, http.StatusOK)
	if st.startedFor != "alex" || scheduledFor != "alex" {
		t.Fatalf("started=%q scheduled=%q", st.startedFor, scheduledFor)
	}
	st.startedFor, scheduledFor = "", ""
	h = wordMeaningCleanupHandler(fakeIdentifier{ok: false}, st, schedule)
	assertUnauthorized(t, h, httptest.NewRequest("POST", "/api/words/meanings/cleanup", nil))
	if st.startedFor != "" || scheduledFor != "" {
		t.Fatal("unauthorized cleanup started")
	}
}

func TestWordsListResumesPendingMeaningCleanup(t *testing.T) {
	st := &fakeWordStore{byUser: map[string][]wordreview.Word{"alex": {{ID: "w1", Word: "facility", Status: wordreview.StatusVerified, MeaningStatus: "pending"}}}}
	var resumed string
	h := wordsListHandler(fakeIdentifier{id: "alex", ok: true}, st, &pipeline.Pipeline{}, nil, func(_ context.Context, user string) { resumed = user })
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/words", nil))
	requireStatus(t, rec, http.StatusOK)
	if resumed != "alex" || !strings.Contains(rec.Body.String(), `"meaningStatus":"pending"`) {
		t.Fatalf("resumed=%q body=%s", resumed, rec.Body.String())
	}
}
