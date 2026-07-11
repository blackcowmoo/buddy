package identity

import (
	"context"
	"net/http"
	"strings"

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
	idToken, err := o.verifier.Verify(r.Context(), raw)
	if err != nil {
		return "", false
	}
	var claims struct {
		Email string `json:"email"`
	}
	if err := idToken.Claims(&claims); err != nil || claims.Email == "" {
		return "", false
	}
	return claims.Email, true
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
