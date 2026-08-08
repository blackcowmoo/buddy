package httpserver

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"buddy/server/internal/llm"
	"buddy/server/internal/pipeline"
	"buddy/server/internal/protocol"
	"buddy/server/internal/store"
)

func deleteSessionRequest(t *testing.T, id string) *http.Request {
	t.Helper()
	req := httptest.NewRequest("DELETE", "/api/sessions/"+id, nil)
	req.SetPathValue("id", id)
	return req
}

// TestSessionDeleteRegeneratesProfileWhenDeletedSessionContributed guards
// the point of this feature: deleting a session that had actually folded a
// study summary into the learner profile (ended, non-empty study summary)
// must trigger a from-scratch rebuild (see
// transport.RunProfileRegenerateInline/EnqueueProfileRegenerateJob) so its
// influence doesn't linger in the profile forever — the whole reason a
// learner deletes a room (e.g. a bad/wrong reply) is to keep it out of
// future study material.
func TestSessionDeleteRegeneratesProfileWhenDeletedSessionContributed(t *testing.T) {
	st := &fakeSessionStore{
		detailMeta: store.SessionMeta{
			Ended:        true,
			StudySummary: []protocol.StudySummarySentence{{English: "Focus on articles.", Translation: "관사에 집중하세요."}},
		},
		detailTurns: []store.Turn{}, // non-nil: makes the fake's SessionDetail succeed
		withStudySummary: []store.SessionMeta{
			{ID: "s2", StudySummary: []protocol.StudySummarySentence{{English: "Remaining session.", Translation: "남은 세션."}}},
		},
	}
	pipe := &pipeline.Pipeline{
		Analysis: []pipeline.Candidate{{Model: "m", LLM: &fakeStudySummaryLLM{complete: func(msgs []llm.Message) (string, error) {
			return "rebuilt profile", nil
		}}}},
	}
	h := sessionDeleteHandler(fakeIdentifier{id: "alex", ok: true}, st, nil, nil, pipe, nil)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, deleteSessionRequest(t, "s1"))

	requireStatus(t, rec, http.StatusNoContent)
	waitForCondition(t, 2*time.Second, func() bool {
		return st.snapshotWithStudySummaryCalls() > 0
	})
	waitForCondition(t, 2*time.Second, func() bool {
		return st.snapshotLearnerProfile("alex") == "rebuilt profile"
	})
}

// TestSessionDeleteDoesNotRegenerateProfileWhenDeletedSessionNeverEnded
// guards against a needless full rebuild: a still-open room (never ended,
// so it never folded anything into the profile at all) being deleted must
// not trigger one.
func TestSessionDeleteDoesNotRegenerateProfileWhenDeletedSessionNeverEnded(t *testing.T) {
	st := &fakeSessionStore{
		detailMeta:  store.SessionMeta{Ended: false},
		detailTurns: []store.Turn{},
	}
	st.learnerProfiles = map[string]string{"alex": "untouched"}
	h := sessionDeleteHandler(fakeIdentifier{id: "alex", ok: true}, st, nil, nil, nil, nil)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, deleteSessionRequest(t, "s1"))

	requireStatus(t, rec, http.StatusNoContent)
	// Nothing async was even kicked off (contributedToProfile is false, so
	// the handler never reaches enqueueOrRunInline at all) — safe to assert
	// synchronously right away, unlike the "did regenerate" test above.
	if calls := st.snapshotWithStudySummaryCalls(); calls != 0 {
		t.Fatalf("ListSessionsWithStudySummary calls = %d, want 0 (session was never ended)", calls)
	}
	if got := st.snapshotLearnerProfile("alex"); got != "untouched" {
		t.Fatalf("learner profile = %q, want untouched", got)
	}
}

// TestSessionDeleteDoesNotRegenerateProfileWhenDeletedSessionHadNoStudySummary
// mirrors the above for the other half of the "contributed" check: ended,
// but nothing was ever flagged (empty study summary), so nothing was ever
// folded in either.
func TestSessionDeleteDoesNotRegenerateProfileWhenDeletedSessionHadNoStudySummary(t *testing.T) {
	st := &fakeSessionStore{
		detailMeta:  store.SessionMeta{Ended: true, StudySummary: nil},
		detailTurns: []store.Turn{},
	}
	st.learnerProfiles = map[string]string{"alex": "untouched"}
	h := sessionDeleteHandler(fakeIdentifier{id: "alex", ok: true}, st, nil, nil, nil, nil)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, deleteSessionRequest(t, "s1"))

	requireStatus(t, rec, http.StatusNoContent)
	if calls := st.snapshotWithStudySummaryCalls(); calls != 0 {
		t.Fatalf("ListSessionsWithStudySummary calls = %d, want 0 (nothing was ever folded in)", calls)
	}
	if got := st.snapshotLearnerProfile("alex"); got != "untouched" {
		t.Fatalf("learner profile = %q, want untouched", got)
	}
}

// TestSessionDeleteDoesNotRegenerateProfileWhenSessionDetailFails documents
// the fail-safe: if the pre-delete SessionDetail check itself errors (e.g.
// the session was already gone), the delete still proceeds, but no rebuild
// is attempted — better to skip a possibly-needed rebuild than to guess.
func TestSessionDeleteDoesNotRegenerateProfileWhenSessionDetailFails(t *testing.T) {
	st := &fakeSessionStore{} // detailTurns/detailErr both zero -> SessionDetail errors
	st.learnerProfiles = map[string]string{"alex": "untouched"}
	h := sessionDeleteHandler(fakeIdentifier{id: "alex", ok: true}, st, nil, nil, nil, nil)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, deleteSessionRequest(t, "s1"))

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 even though the pre-delete check errored", rec.Code)
	}
	if len(st.deleted) != 1 {
		t.Fatalf("session delete should still have happened, deleted = %+v", st.deleted)
	}
	if calls := st.snapshotWithStudySummaryCalls(); calls != 0 {
		t.Fatalf("ListSessionsWithStudySummary calls = %d, want 0", calls)
	}
	if got := st.snapshotLearnerProfile("alex"); got != "untouched" {
		t.Fatalf("learner profile = %q, want untouched", got)
	}
}
