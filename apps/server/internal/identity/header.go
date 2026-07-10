package identity

import "net/http"

// HeaderIdentifier trusts a header set by an upstream auth proxy (e.g.
// oauth2-proxy in front of Dex) that has already authenticated the request —
// commonly X-Auth-Request-Email (oauth2-proxy's --set-xauthrequest) or
// X-Forwarded-Email/-User (--pass-user-headers).
//
// This is only safe if the app is unreachable except through that proxy —
// e.g. a Kubernetes Service with no other ingress path to it. Anyone who can
// reach the app directly could otherwise set this header themselves and
// impersonate any user. Enforcing that boundary is infra (NetworkPolicy /
// Service topology), not this package's job.
type HeaderIdentifier struct {
	Header string // e.g. "X-Auth-Request-Email"
}

func NewHeaderIdentifier(header string) HeaderIdentifier {
	return HeaderIdentifier{Header: header}
}

func (h HeaderIdentifier) Identify(w http.ResponseWriter, r *http.Request) (string, bool) {
	v := r.Header.Get(h.Header)
	return v, v != ""
}
