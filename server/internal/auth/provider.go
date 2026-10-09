package auth

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/open-rails/authkit/iam"
	"github.com/open-rails/authkit/verify"
	helpersauth "github.com/open-rails/helpers/auth"

	"github.com/open-rails/openrails/internal/billingauth"
	"github.com/open-rails/openrails/internal/requestauth"
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
	}, nil
}

// Identity is the identity r's user token acts as, from the same
// verification Authenticate makes: bound to the token's session or device
// key, so a permission check refuses it once that sign-in is revoked.
func (p *Authenticator) Identity(r *http.Request) (helpersauth.Identity, error) {
	if r == nil {
		return helpersauth.Identity{}, billingauth.ErrUnauthenticated
	}
	cl, err := p.claims(r.Context(), r)
	if err != nil {
		return helpersauth.Identity{}, err
	}
	if !cl.IsUser() || cl.IsResourceToken() {
		return helpersauth.Identity{}, billingauth.ErrUnauthenticated
	}
	id := iam.InSession(iam.UserIdentity(cl.UserID), iam.SessionRef{SessionID: cl.SessionID, DeviceKeyID: cl.DeviceKeyID})
	if id.Subject == "" {
		return helpersauth.Identity{}, billingauth.ErrUnauthenticated
	}
	return id, nil
}

// CheckRecentSignIn is AuthKit's Sensitive check for r's user token, with
// helpers/auth RecentSignInChecker's errors.
func (p *Authenticator) CheckRecentSignIn(ctx context.Context, r *http.Request) error {
	cl, err := p.claims(ctx, r)
	if err != nil {
		return err
	}
	sessions, ok := p.verifier.(verify.SessionChecker)
	if !ok {
		return billingauth.ErrRecentSignInUnavailable
	}
	err = sessions.CheckRecentSignIn(ctx, cl)
	e, _ := iam.AsError(err)
	switch {
	case err == nil:
		return nil
	case e != nil && e.Code() == "step_up_required":
		return errors.Join(helpersauth.ErrStepUpRequired, err)
	case errors.Is(err, iam.ErrSessionRevoked):
		return errors.Join(helpersauth.ErrRevoked, err)
	case e != nil && e.Status() == http.StatusForbidden:
		return errors.Join(helpersauth.ErrForbidden, err)
	default:
		return errors.Join(helpersauth.ErrUnavailable, err)
	}
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
