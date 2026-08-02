package httpserver

import (
	"net/http"

	"buddy/server/internal/identity"
)

// meHandler resolves the caller's identity the same way the /ws handshake
// does, so the frontend can show something better than an opaque cookie
// value in its menu (see apps/web/src/lib/me.ts): the OIDC "email" claim
// when identityMode is "oidc", and nothing meaningful otherwise — the
// CookieIdentifier's id is a random per-browser token, not a real identity,
// so the frontend only trusts it when identityMode says it's real.
func meHandler(identityMode string, ident identity.Identifier) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		userID, ok := requireUser(w, r, ident)
		if !ok {
			return
		}
		writeJSON(w, map[string]any{
			"identityMode": identityMode,
			"id":           userID,
		})
	}
}
