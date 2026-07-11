package identity

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/coreos/go-oidc/v3/oidc/oidctest"
)

const testClientID = "buddy"

// newMockDex starts an httptest server that plays Dex's part in the OIDC
// discovery/JWKS handshake, and returns a signer for minting tokens plus the
// server's issuer URL (== srv.URL).
func newMockDex(t *testing.T) (srv *httptest.Server, priv *rsa.PrivateKey) {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	s := &oidctest.Server{
		PublicKeys: []oidctest.PublicKey{
			{PublicKey: priv.Public(), KeyID: "test-key", Algorithm: oidc.RS256},
		},
	}
	srv = httptest.NewServer(s)
	t.Cleanup(srv.Close)
	s.SetIssuer(srv.URL)
	return srv, priv
}

func TestOIDCIdentifierAcceptsValidToken(t *testing.T) {
	srv, priv := newMockDex(t)
	ctx := context.Background()
	ident, err := NewOIDCIdentifier(ctx, srv.URL, testClientID)
	if err != nil {
		t.Fatalf("NewOIDCIdentifier: %v", err)
	}

	claims := `{
		"iss": "` + srv.URL + `",
		"aud": "` + testClientID + `",
		"sub": "user-123",
		"exp": ` + strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10) + `,
		"email": "alex@example.com"
	}`
	token := oidctest.SignIDToken(priv, "test-key", oidc.RS256, claims)

	req := httptest.NewRequest("GET", "/ws", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()

	id, ok := ident.Identify(rec, req)
	if !ok || id != "alex@example.com" {
		t.Fatalf("id, ok = %q, %v; want %q, true", id, ok, "alex@example.com")
	}
}

func TestOIDCIdentifierFailsClosedWhenAuthorizationHeaderMissing(t *testing.T) {
	srv, _ := newMockDex(t)
	ident, err := NewOIDCIdentifier(context.Background(), srv.URL, testClientID)
	if err != nil {
		t.Fatalf("NewOIDCIdentifier: %v", err)
	}

	req := httptest.NewRequest("GET", "/ws", nil)
	rec := httptest.NewRecorder()

	if id, ok := ident.Identify(rec, req); ok || id != "" {
		t.Fatalf("id, ok = %q, %v; want \"\", false when Authorization is absent", id, ok)
	}
}

func TestOIDCIdentifierFailsClosedWhenNotBearerScheme(t *testing.T) {
	srv, _ := newMockDex(t)
	ident, err := NewOIDCIdentifier(context.Background(), srv.URL, testClientID)
	if err != nil {
		t.Fatalf("NewOIDCIdentifier: %v", err)
	}

	req := httptest.NewRequest("GET", "/ws", nil)
	req.Header.Set("Authorization", "Basic dXNlcjpwYXNz")
	rec := httptest.NewRecorder()

	if id, ok := ident.Identify(rec, req); ok || id != "" {
		t.Fatalf("id, ok = %q, %v; want \"\", false for a non-Bearer scheme", id, ok)
	}
}

func TestOIDCIdentifierRejectsWrongAudience(t *testing.T) {
	srv, priv := newMockDex(t)
	ident, err := NewOIDCIdentifier(context.Background(), srv.URL, testClientID)
	if err != nil {
		t.Fatalf("NewOIDCIdentifier: %v", err)
	}

	claims := `{
		"iss": "` + srv.URL + `",
		"aud": "some-other-client",
		"sub": "user-123",
		"exp": ` + strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10) + `,
		"email": "alex@example.com"
	}`
	token := oidctest.SignIDToken(priv, "test-key", oidc.RS256, claims)

	req := httptest.NewRequest("GET", "/ws", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()

	if id, ok := ident.Identify(rec, req); ok || id != "" {
		t.Fatalf("id, ok = %q, %v; want \"\", false when the token's audience doesn't match", id, ok)
	}
}

func TestOIDCIdentifierRejectsExpiredToken(t *testing.T) {
	srv, priv := newMockDex(t)
	ident, err := NewOIDCIdentifier(context.Background(), srv.URL, testClientID)
	if err != nil {
		t.Fatalf("NewOIDCIdentifier: %v", err)
	}

	claims := `{
		"iss": "` + srv.URL + `",
		"aud": "` + testClientID + `",
		"sub": "user-123",
		"exp": ` + strconv.FormatInt(time.Now().Add(-time.Hour).Unix(), 10) + `,
		"email": "alex@example.com"
	}`
	token := oidctest.SignIDToken(priv, "test-key", oidc.RS256, claims)

	req := httptest.NewRequest("GET", "/ws", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()

	if id, ok := ident.Identify(rec, req); ok || id != "" {
		t.Fatalf("id, ok = %q, %v; want \"\", false for an expired token", id, ok)
	}
}

func TestOIDCIdentifierRejectsMissingEmailClaim(t *testing.T) {
	srv, priv := newMockDex(t)
	ident, err := NewOIDCIdentifier(context.Background(), srv.URL, testClientID)
	if err != nil {
		t.Fatalf("NewOIDCIdentifier: %v", err)
	}

	claims := `{
		"iss": "` + srv.URL + `",
		"aud": "` + testClientID + `",
		"sub": "user-123",
		"exp": ` + strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10) + `
	}`
	token := oidctest.SignIDToken(priv, "test-key", oidc.RS256, claims)

	req := httptest.NewRequest("GET", "/ws", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()

	if id, ok := ident.Identify(rec, req); ok || id != "" {
		t.Fatalf("id, ok = %q, %v; want \"\", false when the token has no email claim", id, ok)
	}
}

func TestOIDCIdentifierRejectsTokenSignedByUntrustedKey(t *testing.T) {
	srv, _ := newMockDex(t)
	ident, err := NewOIDCIdentifier(context.Background(), srv.URL, testClientID)
	if err != nil {
		t.Fatalf("NewOIDCIdentifier: %v", err)
	}

	untrusted, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	claims := `{
		"iss": "` + srv.URL + `",
		"aud": "` + testClientID + `",
		"sub": "user-123",
		"exp": ` + strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10) + `,
		"email": "attacker@example.com"
	}`
	token := oidctest.SignIDToken(untrusted, "test-key", oidc.RS256, claims)

	req := httptest.NewRequest("GET", "/ws", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()

	if id, ok := ident.Identify(rec, req); ok || id != "" {
		t.Fatalf("id, ok = %q, %v; want \"\", false for a token signed by a key Dex never advertised", id, ok)
	}
}
