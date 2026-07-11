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

	if rec.Code != http.StatusMovedPermanently {
		t.Fatalf("status = %d, want 301", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/pr/14/" {
		t.Fatalf("Location = %q, want /pr/14/", loc)
	}
}

func TestWithRootPathOutsidePrefix404s(t *testing.T) {
	h := withRootPath("/pr/14", echoPathHandler())

	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestWithRootPathAnotherPrefix404s(t *testing.T) {
	h := withRootPath("/pr/14", echoPathHandler())

	req := httptest.NewRequest("GET", "/pr/15/api/health", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}
