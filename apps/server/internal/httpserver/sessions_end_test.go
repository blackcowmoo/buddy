package httpserver

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"buddy/server/internal/llm"
	"buddy/server/internal/pipeline"
	"buddy/server/internal/store"
)

func postEndRequest(t *testing.T, summary string) *http.Request {
	t.Helper()
	body, err := json.Marshal(map[string]string{"summary": summary})
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	req := httptest.NewRequest("POST", "/api/sessions/s1/end", bytes.NewReader(body))
	req.SetPathValue("id", "s1")
	return req
}

// TestSessionEndHandlerEndsSessionAndMergesProfile guards the primary flow:
// confirming "end this conversation" persists the wrap-up via EndSession AND
// folds it into the learner's persistent cross-session profile (see
// pipeline.UpdateLearnerProfile), not just one or the other.
func TestSessionEndHandlerEndsSessionAndMergesProfile(t *testing.T) {
	pipe := &pipeline.Pipeline{
		Analysis: []pipeline.Candidate{{Model: "m", LLM: &fakeStudySummaryLLM{complete: func(msgs []llm.Message) (string, error) {
			return "merged profile", nil
		}}}},
	}
	st := &fakeSessionStore{learnerProfiles: map[string]string{"alex": "old profile"}}
	h := sessionEndHandler(fakeIdentifier{id: "alex", ok: true}, st, pipe)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, postEndRequest(t, "  focus on third-person -s  "))

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	if len(st.endCalls) != 1 {
		t.Fatalf("EndSession calls = %d, want 1", len(st.endCalls))
	}
	call := st.endCalls[0]
	if call.userID != "alex" || call.sessionID != "s1" || call.studySummary != "focus on third-person -s" {
		t.Fatalf("EndSession call = %+v, want trimmed summary for alex/s1", call)
	}
	if got := st.learnerProfiles["alex"]; got != "merged profile" {
		t.Fatalf("learner profile = %q, want the merged result saved", got)
	}
}

// TestSessionEndHandlerEmptySummarySkipsProfileMerge guards the "no issues
// flagged" case (EndConversationControl's own canned message) from spending
// an LLM call merging nothing into the profile.
func TestSessionEndHandlerEmptySummarySkipsProfileMerge(t *testing.T) {
	calls := 0
	pipe := &pipeline.Pipeline{
		Analysis: []pipeline.Candidate{{Model: "m", LLM: &fakeStudySummaryLLM{complete: func(msgs []llm.Message) (string, error) {
			calls++
			return "should not be called", nil
		}}}},
	}
	st := &fakeSessionStore{}
	h := sessionEndHandler(fakeIdentifier{id: "alex", ok: true}, st, pipe)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, postEndRequest(t, "   "))

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	if len(st.endCalls) != 1 || st.endCalls[0].studySummary != "" {
		t.Fatalf("EndSession calls = %+v, want one call with an empty summary", st.endCalls)
	}
	if calls != 0 {
		t.Fatalf("expected no LLM call when summary is empty, got %d calls", calls)
	}
	if len(st.learnerProfiles) != 0 {
		t.Fatalf("learner profile should be untouched, got %+v", st.learnerProfiles)
	}
}

// TestSessionEndHandlerProfileMergeFailureStillEndsSession guards the
// best-effort contract: a transient failure enriching the cross-session
// profile must not stop the conversation itself from ending, the same
// "side-effect independent of the primary action" convention
// sessionDeleteHandler's audio/recording cascades use.
func TestSessionEndHandlerProfileMergeFailureStillEndsSession(t *testing.T) {
	pipe := &pipeline.Pipeline{
		Analysis: []pipeline.Candidate{{Model: "m", LLM: &fakeStudySummaryLLM{complete: func(msgs []llm.Message) (string, error) {
			return "", errors.New("down")
		}}}},
	}
	st := &fakeSessionStore{}
	h := sessionEndHandler(fakeIdentifier{id: "alex", ok: true}, st, pipe)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, postEndRequest(t, "focus on third-person -s"))

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 even when the profile merge fails", rec.Code)
	}
	if len(st.endCalls) != 1 {
		t.Fatalf("EndSession calls = %d, want 1", len(st.endCalls))
	}
}

func TestSessionEndHandlerNotFoundPropagatesStoreErrNotFound(t *testing.T) {
	st := &fakeSessionStore{endErr: store.ErrNotFound}
	h := sessionEndHandler(fakeIdentifier{id: "alex", ok: true}, st, &pipeline.Pipeline{})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, postEndRequest(t, "summary"))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestSessionEndHandlerUnauthorizedWhenIdentifyFails(t *testing.T) {
	h := sessionEndHandler(fakeIdentifier{ok: false}, &fakeSessionStore{}, &pipeline.Pipeline{})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, postEndRequest(t, "summary"))

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestSessionEndHandlerBadRequestOnInvalidJSON(t *testing.T) {
	h := sessionEndHandler(fakeIdentifier{id: "alex", ok: true}, &fakeSessionStore{}, &pipeline.Pipeline{})

	req := httptest.NewRequest("POST", "/api/sessions/s1/end", bytes.NewReader([]byte("not json")))
	req.SetPathValue("id", "s1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}
