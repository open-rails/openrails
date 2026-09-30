package auth

import (
	"context"
	"net/http"
	"strings"

	"github.com/open-rails/authkit/verify"
	"github.com/open-rails/openrails/internal/requestauth"
	"github.com/open-rails/openrails/pkg/billingauth"
)

// Authenticator is the framework-neutral user authenticator over the control
// plane's AuthKit verifier. Token role snapshots are never carried: merchant
// authority is evaluated live against the merchant's permission group.
type Authenticator struct{ verifier RequestVerifier }

func NewAuthenticator(v RequestVerifier) *Authenticator { return &Authenticator{verifier: v} }

// Authenticate implements billingauth.Authenticator, returning
// billingauth.ErrUnauthenticated when no valid credential is present.
func (p *Authenticator) Authenticate(ctx context.Context, r *http.Request) (billingauth.UserContext, error) {
	if p == nil || p.verifier == nil || r == nil {
		return billingauth.UserContext{}, billingauth.ErrUnauthenticated
	}
	// AuthKit DPoP authenticates delegated subjects, never a local user. Leave
	// its single-use proof for the delegated route's verifier.
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(r.Header.Get("Authorization"))), "dpop ") {
		return billingauth.UserContext{}, billingauth.ErrUnauthenticated
	}
	cl, err := requestauth.Once(ctx, p, func() (verify.Claims, error) { return p.verifier.VerifyRequest(r) })
	if err != nil {
		return billingauth.UserContext{}, err
	}
	return billingauth.UserContext{
		UserID:          cl.UserID,
		Email:           cl.Email,
		EmailVerified:   cl.EmailVerified,
		Username:        cl.Username,
		DiscordUsername: cl.DiscordUsername,
		SessionID:       cl.SessionID,
		Entitlements:    cl.Entitlements,
	}, nil
}
