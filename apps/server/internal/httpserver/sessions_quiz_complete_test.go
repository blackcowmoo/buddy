package httpserver

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func postQuizCompleteRequest(t *testing.T) *http.Request {
	t.Helper()
	req := httptest.NewRequest("POST", "/api/sessions/s1/quiz/complete", nil)
	req.SetPathValue("id", "s1")
	return req
}

// TestSessionQuizCompleteHandlerMarksCompleted guards the primary flow: the
// frontend calls this once a learner answers every quiz question correctly
// (or, for a quiz with no questions at all, taps "내가 읽었음") — either way
// it just sets the store's one-way checkmark, with no server-side re-grading
// (see sessionQuizCompleteHandler's doc comment for why that's fine here).
func TestSessionQuizCompleteHandlerMarksCompleted(t *testing.T) {
	st := &fakeSessionStore{}
	h := sessionQuizCompleteHandler(fakeIdentifier{id: "alex", ok: true}, st)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, postQuizCompleteRequest(t))

	requireStatus(t, rec, http.StatusNoContent)
	if st.snapshotMarkQuizCompletedCalls() != 1 {
		t.Fatalf("MarkQuizCompleted calls = %d, want 1", st.snapshotMarkQuizCompletedCalls())
	}
}

func TestSessionQuizCompleteHandlerInternalErrorOnStoreFailure(t *testing.T) {
	st := &fakeSessionStore{markQuizCompletedErr: errors.New("mysql unreachable")}
	h := sessionQuizCompleteHandler(fakeIdentifier{id: "alex", ok: true}, st)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, postQuizCompleteRequest(t))

	requireStatus(t, rec, http.StatusInternalServerError)
}

func TestSessionQuizCompleteHandlerUnauthorizedWhenIdentifyFails(t *testing.T) {
	h := sessionQuizCompleteHandler(fakeIdentifier{ok: false}, &fakeSessionStore{})

	assertUnauthorized(t, h, postQuizCompleteRequest(t))
}
