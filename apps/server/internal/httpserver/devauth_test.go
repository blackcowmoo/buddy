package httpserver

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"buddy/server/internal/config"
	"buddy/server/internal/identity"
	"buddy/server/internal/pipeline"
)

func TestDevAuthInjectorSetsHeader(t *testing.T) {
	var got string
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("X-Test-Auth")
	})
	h := devAuthInjector("X-Test-Auth", "alex@example.com")(inner)

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))

	if got != "alex@example.com" {
		t.Fatalf("injected header = %q, want alex@example.com", got)
	}
}

func TestDevAuthInjectorOverridesCallerSuppliedValue(t *testing.T) {
	var got string
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("X-Test-Auth")
	})
	h := devAuthInjector("X-Test-Auth", "alex@example.com")(inner)

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("X-Test-Auth", "attacker@example.com")
	h.ServeHTTP(httptest.NewRecorder(), req)

	if got != "alex@example.com" {
		t.Fatalf("injector should override caller-supplied values (like a real auth proxy would), got %q", got)
	}
}

// TestNewGatesDevAuthInjectionToDevOnly is the safety-net regression test:
// DevAuthHeaderValue must never take effect outside dev, no matter how it's
// set, since it would otherwise let anyone impersonate anyone.
func TestNewGatesDevAuthInjectionToDevOnly(t *testing.T) {
	pipe := &pipeline.Pipeline{}

	for _, tc := range []struct {
		name       string
		env        string
		wantStatus int // what /ws returns without any real auth header
	}{
		{"dev with dev-auth set: identity succeeds, unauthorized never returned", "dev", http.StatusOK /* placeholder, checked specially below */},
		{"prod with dev-auth set: identity still fails, 401", "prod", http.StatusUnauthorized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Config{
				Env:                tc.env,
				ViteURL:            "http://localhost:5173",
				IdentityMode:       "header",
				AuthHeader:         "X-Auth-Request-Email",
				DevAuthHeaderValue: "alex@example.com",
			}
			srv := New(cfg, pipe, nil, identity.NewHeaderIdentifier(cfg.AuthHeader), nil)

			req := httptest.NewRequest("GET", "/ws", nil) // no auth header attached
			rec := httptest.NewRecorder()
			srv.Handler.ServeHTTP(rec, req)

			if tc.env == "prod" {
				if rec.Code != http.StatusUnauthorized {
					t.Fatalf("prod: status = %d, want %d (dev-auth must never activate outside dev)", rec.Code, http.StatusUnauthorized)
				}
				return
			}
			// dev: identity must have succeeded (no 401), even though this
			// request carried no real auth header — the injector supplied one.
			if rec.Code == http.StatusUnauthorized {
				t.Fatalf("dev: got 401 — dev-auth injection did not run")
			}
		})
	}
}
