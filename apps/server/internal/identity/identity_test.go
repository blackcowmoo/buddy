package identity

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCookieIdentifierFirstVisitSetsCookie(t *testing.T) {
	ident := NewCookieIdentifier()
	req := httptest.NewRequest("GET", "/ws", nil)
	rec := httptest.NewRecorder()

	id, ok := ident.Identify(rec, req)
	if !ok || id == "" {
		t.Fatalf("expected ok=true and a non-empty ID, got id=%q ok=%v", id, ok)
	}
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != cookieName {
		t.Fatalf("expected one %q cookie, got %+v", cookieName, cookies)
	}
	if cookies[0].Value != id {
		t.Fatalf("cookie value %q != returned id %q", cookies[0].Value, id)
	}
	if !cookies[0].HttpOnly {
		t.Fatal("cookie should be HttpOnly")
	}
}

func TestCookieIdentifierReusesExistingCookie(t *testing.T) {
	ident := NewCookieIdentifier()
	req := httptest.NewRequest("GET", "/ws", nil)
	req.AddCookie(&http.Cookie{Name: cookieName, Value: "existing-id"})
	rec := httptest.NewRecorder()

	id, ok := ident.Identify(rec, req)
	if !ok || id != "existing-id" {
		t.Fatalf("id, ok = %q, %v; want %q, true", id, ok, "existing-id")
	}
	if cookies := rec.Result().Cookies(); len(cookies) != 0 {
		t.Fatalf("should not set a new cookie when one already exists, got %+v", cookies)
	}
}

func TestCookieIdentifierGeneratesDistinctIDs(t *testing.T) {
	ident := NewCookieIdentifier()
	ids := map[string]bool{}
	for i := 0; i < 20; i++ {
		req := httptest.NewRequest("GET", "/ws", nil)
		rec := httptest.NewRecorder()
		id, ok := ident.Identify(rec, req)
		if !ok {
			t.Fatalf("Identify() ok = false on iteration %d", i)
		}
		ids[id] = true
	}
	if len(ids) != 20 {
		t.Fatalf("expected 20 distinct IDs, got %d", len(ids))
	}
}

func TestHeaderIdentifierReadsConfiguredHeader(t *testing.T) {
	ident := NewHeaderIdentifier("X-Auth-Request-Email")
	req := httptest.NewRequest("GET", "/ws", nil)
	req.Header.Set("X-Auth-Request-Email", "alex@example.com")
	rec := httptest.NewRecorder()

	id, ok := ident.Identify(rec, req)
	if !ok || id != "alex@example.com" {
		t.Fatalf("id, ok = %q, %v; want %q, true", id, ok, "alex@example.com")
	}
	if cookies := rec.Result().Cookies(); len(cookies) != 0 {
		t.Fatalf("HeaderIdentifier should never set cookies, got %+v", cookies)
	}
}

func TestHeaderIdentifierFailsClosedWhenHeaderMissing(t *testing.T) {
	ident := NewHeaderIdentifier("X-Auth-Request-Email")
	req := httptest.NewRequest("GET", "/ws", nil)
	rec := httptest.NewRecorder()

	id, ok := ident.Identify(rec, req)
	if ok || id != "" {
		t.Fatalf("id, ok = %q, %v; want \"\", false when the auth header is absent", id, ok)
	}
}

func TestHeaderIdentifierIgnoresUnconfiguredHeaders(t *testing.T) {
	ident := NewHeaderIdentifier("X-Auth-Request-Email")
	req := httptest.NewRequest("GET", "/ws", nil)
	req.Header.Set("X-Forwarded-User", "someone") // a different header
	rec := httptest.NewRecorder()

	if _, ok := ident.Identify(rec, req); ok {
		t.Fatal("expected ok=false: the configured header wasn't set, even though a different one was")
	}
}
