package httpserver

import (
	"net/http/httptest"
	"testing"
	"testing/fstest"
)

func newTestFS() fstest.MapFS {
	return fstest.MapFS{
		"index.html":    {Data: []byte("<html>spa</html>")},
		"assets/app.js": {Data: []byte("console.log('hi')")},
	}
}

func TestSpaHandlerServesRealAsset(t *testing.T) {
	h := spaHandlerFS(newTestFS())
	req := httptest.NewRequest("GET", "/assets/app.js", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if rec.Body.String() != "console.log('hi')" {
		t.Fatalf("body = %q", rec.Body.String())
	}
}

func TestSpaHandlerFallsBackToIndexForUnknownRoute(t *testing.T) {
	h := spaHandlerFS(newTestFS())
	req := httptest.NewRequest("GET", "/some/client/side/route", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if rec.Body.String() != "<html>spa</html>" {
		t.Fatalf("body = %q, want index.html contents", rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Fatalf("Content-Type = %q", ct)
	}
}

func TestSpaHandlerServesIndexAtRoot(t *testing.T) {
	h := spaHandlerFS(newTestFS())
	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Body.String() != "<html>spa</html>" {
		t.Fatalf("body = %q", rec.Body.String())
	}
}

func TestSpaHandlerNotFoundWhenNoIndex(t *testing.T) {
	h := spaHandlerFS(fstest.MapFS{}) // empty FS, no index.html
	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != 404 {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}
