package httpserver

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// requireStatus fails the test if rec's status doesn't match want, including
// the response body for debugging — the single most repeated assertion shape
// across this package's handler tests.
func requireStatus(t *testing.T, rec *httptest.ResponseRecorder, want int) {
	t.Helper()
	if rec.Code != want {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, want, rec.Body.String())
	}
}

// assertUnauthorized runs req through h and requires a 401 — the shared body
// of every "...UnauthorizedWhenIdentifyFails" test in this package (a handler
// built with a fakeIdentifier{ok: false}).
func assertUnauthorized(t *testing.T, h http.Handler, req *http.Request) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	requireStatus(t, rec, http.StatusUnauthorized)
}
