package httpserver

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"buddy/server/internal/store"
)

func postMarkInstantRequest(t *testing.T) *http.Request {
	t.Helper()
	req := httptest.NewRequest("POST", "/api/sessions/s1/instant", nil)
	req.SetPathValue("id", "s1")
	return req
}

// TestSessionMarkInstantHandlerCallsMarkInstant guards the primary flow: the
// frontend calls this once, right after the "ready" WS event hands it a
// brand-new session's minted ID for a room opened in quick/instant mode (see
// apps/web/src/App.tsx's onEvent "ready" case) — no request body, just the
// path-scoped session ID.
func TestSessionMarkInstantHandlerCallsMarkInstant(t *testing.T) {
	st := &fakeSessionStore{}
	h := sessionMarkInstantHandler(fakeIdentifier{id: "alex", ok: true}, st)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, postMarkInstantRequest(t))

	requireStatus(t, rec, http.StatusNoContent)
	if st.snapshotMarkInstantCalls() != 1 {
		t.Fatalf("MarkInstant calls = %d, want 1", st.snapshotMarkInstantCalls())
	}
}

func TestSessionMarkInstantHandlerInternalErrorOnStoreFailure(t *testing.T) {
	st := &fakeSessionStore{markInstantErr: errors.New("mysql unreachable")}
	h := sessionMarkInstantHandler(fakeIdentifier{id: "alex", ok: true}, st)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, postMarkInstantRequest(t))

	requireStatus(t, rec, http.StatusInternalServerError)
}

func TestSessionMarkInstantHandlerUnauthorizedWhenIdentifyFails(t *testing.T) {
	h := sessionMarkInstantHandler(fakeIdentifier{ok: false}, &fakeSessionStore{})

	assertUnauthorized(t, h, postMarkInstantRequest(t))
}

// TestInstantSessionsListHandlerReturnsStoreResult guards the mirror-image
// list endpoint (apps/web/src/pages/InstantSessions.tsx's own list, kept
// separate from the main room list — see sessionsListHandler/ListSessions'
// exclusion of instant rooms).
func TestInstantSessionsListHandlerReturnsStoreResult(t *testing.T) {
	want := []store.SessionMeta{{ID: "s1", Title: "hi", CreatedAt: 1, UpdatedAt: 2}}
	st := &fakeSessionStore{instantSessions: want}
	h := instantSessionsListHandler(fakeIdentifier{id: "alex", ok: true}, st)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/instant-sessions", nil))

	requireStatus(t, rec, http.StatusOK)
	var got []store.SessionMeta
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(got) != 1 || got[0].ID != "s1" {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestInstantSessionsListHandlerInternalErrorOnStoreFailure(t *testing.T) {
	st := &fakeSessionStore{instantSessionsErr: errors.New("mysql unreachable")}
	h := instantSessionsListHandler(fakeIdentifier{id: "alex", ok: true}, st)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/instant-sessions", nil))

	requireStatus(t, rec, http.StatusInternalServerError)
}

func TestInstantSessionsListHandlerUnauthorizedWhenIdentifyFails(t *testing.T) {
	h := instantSessionsListHandler(fakeIdentifier{ok: false}, &fakeSessionStore{})

	assertUnauthorized(t, h, httptest.NewRequest("GET", "/api/instant-sessions", nil))
}
