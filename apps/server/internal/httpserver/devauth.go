package httpserver

import "net/http"

// devAuthInjector simulates an upstream auth proxy (oauth2-proxy + Dex)
// locally: it force-sets header to value on every incoming request before
// identity.HeaderIdentifier reads it, overwriting any client-supplied value —
// the same thing a real auth proxy does to prevent spoofing. Only ever wired
// in when cfg.IsDev() (see New) — structurally impossible to activate in a
// prod deployment regardless of what DevAuthHeaderValue is set to.
func devAuthInjector(header, value string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r.Header.Set(header, value)
			next.ServeHTTP(w, r)
		})
	}
}
