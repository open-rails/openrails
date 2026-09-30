package auth

import (
	"context"
	"net/http"
	"strings"

	"github.com/open-rails/authkit/iam"
	"github.com/open-rails/authkit/verify"

	"github.com/open-rails/openrails/internal/requestauth"
	"github.com/open-rails/openrails/pkg/billingauth"
)

// Authenticator is the framework-neutral user authenticator over the control
// plane's AuthKit verifier. Token role snapshots are never carried: merchant
// authority is evaluated live against the merchant's permission group.
type Authenticator struct{ verifier verify.Authenticator }

func NewAuthenticator(v verify.Authenticator) *Authenticator { return &Authenticator{verifier: v} }

// Authenticate implements billingauth.Authenticator, returning
// billingauth.ErrUnauthenticated when no valid credential is present.
func (p *Authenticator) Authenticate(ctx context.Context, r *http.Request) (billingauth.UserContext, error) {
	cl, err := p.claims(ctx, r)
	if err != nil {
		return billingauth.UserContext{}, err
	}
	return billingauth.UserContext{
		UserID:        cl.UserID,
		Email:         cl.Email,
		EmailVerified: cl.EmailVerified,
		Username:      cl.Username,
		SessionID:     cl.SessionID,
		Entitlements:  cl.Entitlements,
	}, nil
}

// Actor is the actor r's user token acts as (verify.ActorFromClaims), from the
// same verification Authenticate makes: bound to the token's session, so a
// permission check refuses it once that sign-in is revoked.
func (p *Authenticator) Actor(r *http.Request) (iam.Actor, error) {
	if r == nil {
		return iam.Actor{}, billingauth.ErrUnauthenticated
	}
	cl, err := p.claims(r.Context(), r)
	if err != nil {
		return iam.Actor{}, err
	}
	actor, ok := verify.ActorFromClaims(cl)
	if !ok || actor.Kind() != iam.ActorUser {
		return iam.Actor{}, billingauth.ErrUnauthenticated
	}
	return actor, nil
}

// claims verifies r once per request.
func (p *Authenticator) claims(ctx context.Context, r *http.Request) (verify.Claims, error) {
	if p == nil || p.verifier == nil || r == nil {
		return verify.Claims{}, billingauth.ErrUnauthenticated
	}
	// AuthKit DPoP authenticates delegated subjects, never a local user. Leave
	// its single-use proof for the delegated route's verifier.
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(r.Header.Get("Authorization"))), "dpop ") {
		return verify.Claims{}, billingauth.ErrUnauthenticated
	}
	return requestauth.Once(ctx, p, func() (verify.Claims, error) { return p.verifier.VerifyRequest(r) })
}
