package auth

import (
	"context"
	"net/http"
	"strings"

	authhttp "github.com/open-rails/authkit/http"
	"github.com/open-rails/openrails/pkg/billingauth"
	log "github.com/sirupsen/logrus"
)

// Authenticator is the gin-free auth boundary backed by an AuthKit JWT verifier.
// It implements billingauth.Authenticator. The gin Required()/Optional()
// Gin middleware wraps it from pkg/authprovider/ginauth so this gin-free core
// and its transitive importers do not pull in github.com/gin-gonic/gin (#285).
type Authenticator struct {
	verifier Verifier
}

// NewAuthenticator builds the gin-free AuthKit-backed Authenticator around a
// verifier supplied by the caller. Standalone passes the control-plane verifier;
// embedded hosts may pass their own explicit verifier.
func NewAuthenticator(v Verifier) *Authenticator {
	return &Authenticator{verifier: v}
}

// Authenticate implements billingauth.Authenticator so the AuthKit-backed
// provider can drive the gin-free embedded surface (issue #282). It verifies the
// bearer token and returns the resulting UserContext, returning
// billingauth.ErrUnauthenticated when no/invalid credential is present.
func (p *Authenticator) Authenticate(_ context.Context, r *http.Request) (billingauth.UserContext, error) {
	if p == nil || p.verifier == nil {
		return billingauth.UserContext{}, billingauth.ErrUnauthenticated
	}
	token := bearerToken(r.Header.Get("Authorization"))
	if token == "" {
		return billingauth.UserContext{}, billingauth.ErrUnauthenticated
	}
	raw, err := p.verifier.Verify(token)
	if err != nil {
		log.WithError(err).Warn("jwt verification failed")
		return billingauth.UserContext{}, err
	}
	return userContextFromClaims(raw), nil
}

func bearerToken(header string) string {
	header = strings.TrimSpace(header)
	if header == "" {
		return ""
	}
	const prefix = "Bearer "
	if !strings.HasPrefix(strings.ToLower(header), strings.ToLower(prefix)) {
		return ""
	}
	return strings.TrimSpace(header[len(prefix):])
}

func userContextFromClaims(cl authhttp.Claims) billingauth.UserContext {
	return billingauth.UserContext{
		UserID:          cl.UserID,
		Email:           cl.Email,
		EmailVerified:   cl.EmailVerified,
		Username:        cl.Username,
		DiscordUsername: cl.DiscordUsername,
		SessionID:       cl.SessionID,
		Roles:           cl.Roles,
		Entitlements:    cl.Entitlements,
		// #567: the permission-group model has no legacy tenant persona, so a user
		// access token carries no group slug/group roles. Merchant-admin authority
		// is a role on the merchant permission-group, evaluated live by
		// HasAdminPermission against the merchant the route resolves — not read from the token. Merchant is
		// left empty here; it is populated only on the in-process/delegated paths
		// that bind an explicit merchant (see internal/http/routes + middleware).
	}
}
