package httpserver

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

type fakeIdentifier struct {
	id string
	ok bool
}

func (f fakeIdentifier) Identify(http.ResponseWriter, *http.Request) (string, bool) {
	return f.id, f.ok
}

func TestMeHandlerReturnsResolvedIdentity(t *testing.T) {
	h := meHandler("oidc", fakeIdentifier{id: "alex@example.com", ok: true})

	req := httptest.NewRequest("GET", "/api/me", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	requireStatus(t, rec, http.StatusOK)
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("bad JSON body: %v", err)
	}
	if body["identityMode"] != "oidc" || body["id"] != "alex@example.com" {
		t.Fatalf("body = %+v, want identityMode=oidc id=alex@example.com", body)
	}
}

func TestMeHandlerReportsCookieMode(t *testing.T) {
	h := meHandler("cookie", fakeIdentifier{id: "deadbeef", ok: true})

	req := httptest.NewRequest("GET", "/api/me", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("bad JSON body: %v", err)
	}
	if body["identityMode"] != "cookie" {
		t.Fatalf("identityMode = %q, want cookie", body["identityMode"])
	}
}

func TestMeHandlerUnauthorizedWhenIdentifyFails(t *testing.T) {
	h := meHandler("oidc", fakeIdentifier{ok: false})

	req := httptest.NewRequest("GET", "/api/me", nil)
	assertUnauthorized(t, h, req)
}
