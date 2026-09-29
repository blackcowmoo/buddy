package httpserver

import (
	"context"
	"encoding/json"
	"errors"
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
	startErr   error
}

func (s *meaningHTTPStore) PendingMeanings(ctx context.Context, userID string) ([]wordreview.Word, error) {
	return s.List(ctx, userID)
}

func (s *meaningHTTPStore) StartMeaningCleanup(_ context.Context, user string) error {
	s.startedFor = user
	if s.startErr != nil {
		return s.startErr
	}
	for i := range s.byUser[user] {
		word := &s.byUser[user][i]
		if word.Status == wordreview.StatusVerified && word.MeaningVersion < wordreview.CurrentMeaningVersion && word.MeaningTargetVersion < wordreview.CurrentMeaningVersion {
			word.MeaningStatus = wordreview.MeaningPending
			word.MeaningTargetVersion = wordreview.CurrentMeaningVersion
			word.MeaningRevision++
		}
	}
	return nil
}
func (s *meaningHTTPStore) SaveMeaning(context.Context, wordreview.Word, string) error { return nil }
func (s *meaningHTTPStore) FailMeaning(context.Context, wordreview.Word, string) error { return nil }

func TestWordsListAutomaticallySchedulesMeaningRefreshForAuthenticatedUser(t *testing.T) {
	old := wordreview.Word{ID: "w1", Word: "facility", Status: wordreview.StatusVerified, ResearchStatus: wordreview.ResearchConfirmed,
		MeaningVersion: wordreview.CurrentMeaningVersion - 1, MeaningTargetVersion: wordreview.CurrentMeaningVersion - 1, MeaningStatus: wordreview.MeaningConfirmed}
	st := &meaningHTTPStore{fakeWordStore: &fakeWordStore{byUser: map[string][]wordreview.Word{"alex": {old}, "sam": {old}}}}
	var scheduledFor string
	schedule := func(_ context.Context, user string) { scheduledFor = user }
	h := wordsListHandler(fakeIdentifier{id: "alex", ok: true}, st, nil, nil, schedule)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/words?userID=sam", nil))
	requireStatus(t, rec, http.StatusOK)
	if st.startedFor != "alex" || scheduledFor != "alex" {
		t.Fatalf("started=%q scheduled=%q", st.startedFor, scheduledFor)
	}
	var got struct {
		Words []wordItem `json:"words"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Words) != 1 || got.Words[0].MeaningStatus != wordreview.MeaningPending || got.Words[0].MeaningVersion != old.MeaningVersion || got.Words[0].MeaningTargetVersion != wordreview.CurrentMeaningVersion || got.Words[0].MeaningRevision != 1 {
		t.Fatalf("refresh must be visible in the first response without advancing the confirmed version: %+v", got)
	}
	if st.byUser["sam"][0].MeaningStatus != wordreview.MeaningConfirmed {
		t.Fatal("refreshed another user's words")
	}
	st.startedFor, scheduledFor = "", ""
	h = wordsListHandler(fakeIdentifier{ok: false}, st, nil, nil, schedule)
	assertUnauthorized(t, h, httptest.NewRequest("GET", "/api/words", nil))
	if st.startedFor != "" || scheduledFor != "" {
		t.Fatal("unauthorized cleanup started")
	}
}

func TestWordsListMeaningRefreshFailureIsReported(t *testing.T) {
	st := &meaningHTTPStore{fakeWordStore: &fakeWordStore{}, startErr: errors.New("unavailable")}
	h := wordsListHandler(fakeIdentifier{id: "alex", ok: true}, st, nil, nil, func(context.Context, string) { t.Fatal("scheduled without persisted intent") })
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/words", nil))
	requireStatus(t, rec, http.StatusInternalServerError)
}

func TestWordsListDoesNotScheduleCompletedOrFailedMeaningRefresh(t *testing.T) {
	for _, status := range []string{wordreview.MeaningDone, wordreview.MeaningFailed, wordreview.MeaningConfirmed} {
		t.Run(status, func(t *testing.T) {
			word := wordreview.Word{ID: "w1", Status: wordreview.StatusVerified, MeaningStatus: status, MeaningTargetVersion: wordreview.CurrentMeaningVersion}
			st := &meaningHTTPStore{fakeWordStore: &fakeWordStore{byUser: map[string][]wordreview.Word{"alex": {word}}}}
			h := wordsListHandler(fakeIdentifier{id: "alex", ok: true}, st, nil, nil, func(context.Context, string) { t.Fatal("scheduled finished work") })
			for range 2 {
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/words", nil))
				requireStatus(t, rec, http.StatusOK)
			}
			if st.byUser["alex"][0].MeaningStatus != status {
				t.Fatal("list changed a learner decision")
			}
		})
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

type meaningSelectionHTTPStore struct {
	*fakeWordStore
	user, id string
	choice   wordreview.MeaningChoice
	revision int
	result   wordreview.Word
	err      error
}

func (s *meaningSelectionHTTPStore) SelectMeaning(_ context.Context, user, id string, choice wordreview.MeaningChoice, revision int) (wordreview.Word, error) {
	s.user, s.id, s.choice, s.revision = user, id, choice, revision
	return s.result, s.err
}

func TestMeaningSelectionHandlerScopesChoiceAndSchedulesOnlyRetry(t *testing.T) {
	for _, choice := range []wordreview.MeaningChoice{wordreview.MeaningChoiceCleaned, wordreview.MeaningChoiceOriginal} {
		t.Run(string(choice), func(t *testing.T) {
			status := wordreview.MeaningConfirmed
			if choice == wordreview.MeaningChoiceOriginal {
				status = wordreview.MeaningPending
			}
			st := &meaningSelectionHTTPStore{fakeWordStore: &fakeWordStore{}, result: wordreview.Word{
				ID: "w1", UserID: "alex", Meaning: "생산 시설", MeaningStatus: status, MeaningRevision: 7,
			}}
			var scheduled string
			h := wordMeaningSelectionHandler(fakeIdentifier{id: "alex", ok: true}, st, func(_ context.Context, user string) { scheduled = user })
			r := httptest.NewRequest("POST", "/api/words/w1/meaning", strings.NewReader(`{"userID":"sam","choice":"`+string(choice)+`","revision":6}`))
			r.SetPathValue("id", "w1")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, r)
			requireStatus(t, rec, http.StatusOK)
			if st.user != "alex" || st.id != "w1" || st.choice != choice || st.revision != 6 {
				t.Fatalf("selection=%+v", st)
			}
			var got wordItem
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if got.ID != "w1" || got.MeaningStatus != status || got.MeaningRevision != 7 {
				t.Fatalf("response=%+v", got)
			}
			if choice == wordreview.MeaningChoiceOriginal && scheduled != "alex" || choice == wordreview.MeaningChoiceCleaned && scheduled != "" {
				t.Fatalf("scheduled=%q for %s", scheduled, choice)
			}
		})
	}
}

func TestMeaningSelectionHandlerRejectsInvalidRequests(t *testing.T) {
	for _, tc := range []struct {
		name, body                string
		unauthorized, unsupported bool
		storeErr                  error
		status                    int
	}{
		{name: "unauthorized", unauthorized: true, status: http.StatusUnauthorized},
		{name: "unsupported", unsupported: true, status: http.StatusServiceUnavailable},
		{name: "invalid json", body: `{`, status: http.StatusBadRequest},
		{name: "unknown choice", body: `{"choice":"other","revision":1}`, status: http.StatusBadRequest},
		{name: "missing revision", body: `{"choice":"cleaned"}`, status: http.StatusBadRequest},
		{name: "negative revision", body: `{"choice":"cleaned","revision":-1}`, status: http.StatusBadRequest},
		{name: "stale result", storeErr: wordreview.ErrMeaningConflict, status: http.StatusConflict},
		{name: "store failure", storeErr: errors.New("unavailable"), status: http.StatusInternalServerError},
		{name: "missing word", status: http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := &meaningSelectionHTTPStore{fakeWordStore: &fakeWordStore{}, err: tc.storeErr}
			var words wordreview.Store = st
			if tc.unsupported {
				words = st.fakeWordStore
			}
			body := tc.body
			if body == "" {
				body = `{"choice":"cleaned","revision":1}`
			}
			h := wordMeaningSelectionHandler(fakeIdentifier{id: "alex", ok: !tc.unauthorized}, words, func(context.Context, string) { t.Error("scheduled invalid request") })
			r := httptest.NewRequest("POST", "/api/words/w1/meaning", strings.NewReader(body))
			r.SetPathValue("id", "w1")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, r)
			requireStatus(t, rec, tc.status)
			if (tc.status == http.StatusBadRequest || tc.unauthorized) && st.user != "" {
				t.Fatal("invalid request changed meaning")
			}
		})
	}
}
