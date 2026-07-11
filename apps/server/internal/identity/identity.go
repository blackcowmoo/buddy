// Package identity resolves a stable user ID for a connection. It exists so
// internal/store has someone to key Profiles by. CookieIdentifier is the
// zero-setup local-dev default; OIDCIdentifier (oidc.go) is real identity —
// a Dex-issued JWT verified directly against Dex — see main.go's
// buildIdentity for the switch.
package identity

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"time"
)

const cookieName = "buddy_uid"

// Identifier resolves an opaque, stable user ID for a request. ok is false
// when identity couldn't be established (e.g. an auth proxy header is
// missing) — the caller must refuse the connection rather than fall back to
// a shared/empty ID.
type Identifier interface {
	Identify(w http.ResponseWriter, r *http.Request) (userID string, ok bool)
}

// CookieIdentifier is NOT authentication: it hands out a random ID on first
// visit and remembers it via a long-lived cookie. It exists so per-user
// persistent memory (internal/store) has a stable key to work with without
// standing up a real auth proxy — intended for local dev, not production.
type CookieIdentifier struct{}

func NewCookieIdentifier() CookieIdentifier { return CookieIdentifier{} }

func (CookieIdentifier) Identify(w http.ResponseWriter, r *http.Request) (string, bool) {
	if c, err := r.Cookie(cookieName); err == nil && c.Value != "" {
		return c.Value, true
	}
	id := randID()
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    id,
		Path:     "/",
		Expires:  time.Now().AddDate(10, 0, 0),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
	return id, true
}

func randID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
