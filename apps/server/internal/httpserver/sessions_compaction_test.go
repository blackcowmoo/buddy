package httpserver

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"buddy/server/internal/llm"
	"buddy/server/internal/store"
)

func TestSessionCompactionReturnsSummaryAndCounts(t *testing.T) {
	st := &fakeSessionStore{
		profile: store.Profile{
			Summary: "Learner enjoys travel topics; struggles with past perfect tense.",
			Recent: []llm.Message{
				{Role: llm.RoleUser, Content: "hi"},
				{Role: llm.RoleAssistant, Content: "hello"},
			},
		},
		lastTurnVal: 42,
	}
	h := sessionCompactionHandler(fakeIdentifier{id: "alex", ok: true}, st)

	req := httptest.NewRequest("GET", "/api/sessions/s1/compaction", nil)
	req.SetPathValue("id", "s1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	requireStatus(t, rec, http.StatusOK)
	var body struct {
		Summary        string `json:"summary"`
		RecentMessages int    `json:"recentMessages"`
		TotalTurns     int    `json:"totalTurns"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Summary != st.profile.Summary {
		t.Errorf("summary = %q, want %q", body.Summary, st.profile.Summary)
	}
	if body.RecentMessages != 2 {
		t.Errorf("recentMessages = %d, want 2", body.RecentMessages)
	}
	if body.TotalTurns != 42 {
		t.Errorf("totalTurns = %d, want 42", body.TotalTurns)
	}
}

func TestSessionCompactionUnauthorizedWhenIdentifyFails(t *testing.T) {
	h := sessionCompactionHandler(fakeIdentifier{ok: false}, &fakeSessionStore{})

	req := httptest.NewRequest("GET", "/api/sessions/s1/compaction", nil)
	req.SetPathValue("id", "s1")
	assertUnauthorized(t, h, req)
}

func TestSessionCompactionPropagatesLoadError(t *testing.T) {
	st := &fakeSessionStore{profileErr: errors.New("boom")}
	h := sessionCompactionHandler(fakeIdentifier{id: "alex", ok: true}, st)

	req := httptest.NewRequest("GET", "/api/sessions/s1/compaction", nil)
	req.SetPathValue("id", "s1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	requireStatus(t, rec, http.StatusInternalServerError)
}
