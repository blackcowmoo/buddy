package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"buddy/server/internal/wordreview"
)

type researchSelectionHTTPStore struct {
	*fakeWordStore
	userID, id string
	revision   int
	choice     wordreview.ResearchSuggestion
	result     wordreview.Word
	selectErr  error
}

func (s *researchSelectionHTTPStore) SelectResearch(_ context.Context, userID, id string, revision int, choice wordreview.ResearchSuggestion) (wordreview.Word, error) {
	s.userID, s.id, s.revision, s.choice = userID, id, revision, choice
	return s.result, s.selectErr
}

func TestWordResearchSelectionMapsPersistedChoiceAndErrors(t *testing.T) {
	choice := wordreview.ResearchSuggestion{Word: "recapitalize", Meaning: "재자본화하다", Example: "Investors recapitalize the bank."}
	for _, tc := range []struct {
		name   string
		result wordreview.Word
		err    error
		status int
	}{
		{"selected", wordreview.Word{ID: "w1", Word: choice.Word, Meaning: choice.Meaning, Example: choice.Example, Status: wordreview.StatusVerified, ReviewCount: 7, ResearchRevision: 2}, nil, http.StatusOK},
		{"stale or duplicate", wordreview.Word{}, wordreview.ErrResearchConflict, http.StatusConflict},
		{"missing", wordreview.Word{}, nil, http.StatusNotFound},
		{"store error", wordreview.Word{}, errors.New("database unavailable"), http.StatusInternalServerError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := &researchSelectionHTTPStore{fakeWordStore: &fakeWordStore{}, result: tc.result, selectErr: tc.err}
			h := wordResearchSelectionHandler(fakeIdentifier{id: "alex", ok: true}, st)
			req := httptest.NewRequest("POST", "/api/words/w1/research/select?userID=someone-else", strings.NewReader(`{"revision":2,"suggestion":{"word":"recapitalize","meaning":"재자본화하다","example":"Investors recapitalize the bank."}}`))
			req.SetPathValue("id", "w1")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			requireStatus(t, rec, tc.status)
			if st.userID != "alex" || st.id != "w1" || st.revision != 2 || st.choice != choice {
				t.Fatalf("wrong selection arguments: %+v", st)
			}
			if tc.status == http.StatusOK {
				var got wordItem
				if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || got.ID != "w1" || got.Example != choice.Example || got.ResearchRevision != 2 || got.ReviewCount != 7 {
					t.Fatalf("selection response=%+v err=%v", got, err)
				}
			}
		})
	}
}

func TestWordResearchSelectionRequiresAuthentication(t *testing.T) {
	st := &researchSelectionHTTPStore{fakeWordStore: &fakeWordStore{}}
	h := wordResearchSelectionHandler(fakeIdentifier{ok: false}, st)
	assertUnauthorized(t, h, httptest.NewRequest("POST", "/api/words/w1/research/select", strings.NewReader(`{}`)))
	if st.id != "" {
		t.Fatal("unauthorized request reached selection")
	}
}

func TestWordResearchPendingRequestDoesNotRestartGeneration(t *testing.T) {
	w := wordreview.Word{ID: "w1", Status: wordreview.StatusRejected, ResearchStatus: wordreview.ResearchPending, ResearchRevision: 3}
	st := &fakeWordStore{byUser: map[string][]wordreview.Word{"alex": {w}}}
	h := wordResearchHandler(fakeIdentifier{id: "alex", ok: true}, st, nil, nil)
	req := httptest.NewRequest("POST", "/api/words/w1/research", nil)
	req.SetPathValue("id", "w1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	requireStatus(t, rec, http.StatusOK)
	var got wordItem
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || got.ResearchRevision != 3 || got.ResearchStatus != wordreview.ResearchPending {
		t.Fatalf("pending response=%+v err=%v", got, err)
	}
}

func TestWordResearchWaitsForVerificationAndMeaningReview(t *testing.T) {
	for _, w := range []wordreview.Word{
		{ID: "w1", Status: wordreview.StatusPending},
		{ID: "w1", Status: wordreview.StatusVerified, MeaningStatus: wordreview.MeaningPending},
	} {
		st := &fakeWordStore{byUser: map[string][]wordreview.Word{"alex": {w}}}
		h := wordResearchHandler(fakeIdentifier{id: "alex", ok: true}, st, nil, nil)
		req := httptest.NewRequest("POST", "/api/words/w1/research", nil)
		req.SetPathValue("id", "w1")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		requireStatus(t, rec, http.StatusConflict)
	}
}

func TestWordResearchCannotBeConfirmedWhileGenerating(t *testing.T) {
	st := &fakeWordStore{byUser: map[string][]wordreview.Word{"alex": {{ID: "w1", Status: wordreview.StatusRejected, ResearchStatus: wordreview.ResearchPending}}}}
	h := wordResearchConfirmHandler(fakeIdentifier{id: "alex", ok: true}, st)
	req := httptest.NewRequest("POST", "/api/words/w1/research/confirm", nil)
	req.SetPathValue("id", "w1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	requireStatus(t, rec, http.StatusConflict)
}
