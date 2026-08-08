package httpserver

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func echoPathHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(r.URL.Path))
	})
}

func TestWithRootPathEmptyPassesThrough(t *testing.T) {
	h := withRootPath("", echoPathHandler())

	req := httptest.NewRequest("GET", "/api/health", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK || rec.Body.String() != "/api/health" {
		t.Fatalf("status=%d body=%q, want 200 /api/health", rec.Code, rec.Body.String())
	}
}

func TestWithRootPathStripsPrefixBeforeInnerHandler(t *testing.T) {
	h := withRootPath("/pr/14", echoPathHandler())

	req := httptest.NewRequest("GET", "/pr/14/api/health", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK || rec.Body.String() != "/api/health" {
		t.Fatalf("status=%d body=%q, want 200 /api/health", rec.Code, rec.Body.String())
	}
}

func TestWithRootPathServesPrefixRoot(t *testing.T) {
	h := withRootPath("/pr/14", echoPathHandler())

	req := httptest.NewRequest("GET", "/pr/14/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK || rec.Body.String() != "/" {
		t.Fatalf("status=%d body=%q, want 200 /", rec.Code, rec.Body.String())
	}
}

func TestWithRootPathRedirectsBareRoot(t *testing.T) {
	h := withRootPath("/pr/14", echoPathHandler())

	req := httptest.NewRequest("GET", "/pr/14", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	requireStatus(t, rec, http.StatusMovedPermanently)
	if loc := rec.Header().Get("Location"); loc != "/pr/14/" {
		t.Fatalf("Location = %q, want /pr/14/", loc)
	}
}

func TestWithRootPathOutsidePrefix404s(t *testing.T) {
	h := withRootPath("/pr/14", echoPathHandler())

	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	requireStatus(t, rec, http.StatusNotFound)
}

func TestWithRootPathAnotherPrefix404s(t *testing.T) {
	h := withRootPath("/pr/14", echoPathHandler())

	req := httptest.NewRequest("GET", "/pr/15/api/health", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	requireStatus(t, rec, http.StatusNotFound)
}

func TestRegisterStalePRRedirectSendsStaleDeploymentHome(t *testing.T) {
	mux := http.NewServeMux()
	mux.Handle("/", echoPathHandler()) // stand-in for the SPA catch-all
	registerStalePRRedirect(mux, "")

	req := httptest.NewRequest("GET", "/pr/16/some/deep/path", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	requireStatus(t, rec, http.StatusFound)
	if loc := rec.Header().Get("Location"); loc != "/" {
		t.Fatalf("Location = %q, want /", loc)
	}
}

func TestRegisterStalePRRedirectSkippedUnderRootPath(t *testing.T) {
	mux := http.NewServeMux()
	mux.Handle("/", echoPathHandler())
	registerStalePRRedirect(mux, "/pr/14")

	req := httptest.NewRequest("GET", "/pr/16", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK || rec.Body.String() != "/pr/16" {
		t.Fatalf("status=%d body=%q, want 200 /pr/16 (no redirect under a RootPath deployment)", rec.Code, rec.Body.String())
	}
}
