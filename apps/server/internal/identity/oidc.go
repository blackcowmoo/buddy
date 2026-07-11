package identity

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
)

// OIDCIdentifier verifies the JWT carried in the Authorization: Bearer
// header directly against Dex's OIDC discovery/JWKS endpoint — Dex signs the
// token, so the signature itself is what's trusted here, not network
// topology (contrast with the trust-a-proxy-header approach). The verified
// token's "email" claim becomes the stable user ID.
type OIDCIdentifier struct {
	verifier *oidc.IDTokenVerifier
}

// NewOIDCIdentifier fetches Dex's discovery document once (issuerURL +
// "/.well-known/openid-configuration") and builds a verifier scoped to
// clientID, the audience every accepted token must carry.
func NewOIDCIdentifier(ctx context.Context, issuerURL, clientID string) (OIDCIdentifier, error) {
	provider, err := oidc.NewProvider(ctx, issuerURL)
	if err != nil {
		return OIDCIdentifier{}, err
	}
	return OIDCIdentifier{
		verifier: provider.Verifier(&oidc.Config{ClientID: clientID}),
	}, nil
}

func (o OIDCIdentifier) Identify(w http.ResponseWriter, r *http.Request) (string, bool) {
	raw, ok := bearerToken(r)
	if !ok {
		return "", false
	}
	email, _, err := o.verify(r.Context(), raw)
	return email, err == nil
}

// verify checks raw against Dex's keys and returns the token's email claim
// and expiry. Split out from Identify so CachedOIDCIdentifier (cache.go) can
// reuse the same check for both its initial verification and its background
// revalidation, without duplicating claim parsing.
func (o OIDCIdentifier) verify(ctx context.Context, raw string) (email string, expiry time.Time, err error) {
	idToken, err := o.verifier.Verify(ctx, raw)
	if err != nil {
		return "", time.Time{}, err
	}
	var claims struct {
		Email string `json:"email"`
	}
	if err := idToken.Claims(&claims); err != nil {
		return "", time.Time{}, err
	}
	if claims.Email == "" {
		return "", time.Time{}, errors.New("oidc: token has no email claim")
	}
	return claims.Email, idToken.Expiry, nil
}

func bearerToken(r *http.Request) (string, bool) {
	const prefix = "Bearer "
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, prefix) {
		return "", false
	}
	token := strings.TrimPrefix(auth, prefix)
	return token, token != ""
}
