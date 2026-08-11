package auth

import (
	"context"
	"net/http"
	"strings"

	"github.com/open-rails/authkit/verify"
	"github.com/open-rails/openrails/pkg/billingauth"
	log "github.com/sirupsen/logrus"
)

// PermissionResolver resolves the acting principal's OpenRails permissions
// from the verified claims AND the live request (#918). It supersedes the
// role→permission mapping for hosts whose grant is not a function of the
// token: doujins' merchant-admin authority is a DB read scoped to the admin
// path, which a func([]string) []string cannot express. An error rejects the
// request fail-closed; a host that wants a softer fallback returns the reduced
// permission set with a nil error.
type PermissionResolver func(ctx context.Context, r *http.Request, cl verify.Claims) ([]string, error)

// DelegatedAuthenticator is the AuthKit-verifier-backed
// billingauth.DelegatedAuthenticator: the delegated twin of Authenticator
// (#913). It verifies the host's bearer token and maps the claims onto a
// DelegatedPrincipal pinned to ONE explicit merchant — the embedding engine's
// bound merchant, supplied at construction, NEVER anything read from the
// caller's token. (tensorhub's hand-rolled bridge pinned the CALLER's org
// UUID instead, scoping every request's RLS to a nonexistent merchant —
// th#1765.)
type DelegatedAuthenticator struct {
	cfg DelegatedConfig
}

// DelegatedConfig is DelegatedAuthenticator's construction input.
type DelegatedConfig struct {
	// Verifier verifies the request's credential (required).
	Verifier RequestVerifier
	// MerchantID is the engine's bound merchant, pinned for every principal.
	MerchantID string
	// MerchantSlug is that merchant's slug (optional). It is load-bearing on
	// the customer-treasury surface: or#916 lets a merchant-admin principal
	// name the merchant's own treasury account by slug, and an empty slug
	// leaves the uuid as the only address that resolves.
	MerchantSlug string
	// Issuer overrides the audit issuer stamped on the principal. Empty means
	// the verified token's own `iss`. A host whose customer rows are keyed to
	// one canonical issuer (openrails.SelfIssuer) stamps that instead.
	Issuer string
	// Admit is the optional per-request admission veto (#918).
	Admit Admission
	// Permissions resolves the principal's permissions from the claims and the
	// request. Nil grants nothing beyond the grant-free /v1/me surface.
	Permissions PermissionResolver
}

// NewDelegatedAuthenticator builds the AuthKit-backed DelegatedAuthenticator.
// Most hosts should use the pkg/embedded/authkit constructors, which wire the
// verifier and the canonical permissions.ForRoles preset.
func NewDelegatedAuthenticator(cfg DelegatedConfig) *DelegatedAuthenticator {
	return &DelegatedAuthenticator{cfg: cfg}
}

// AuthenticateDelegated implements billingauth.DelegatedAuthenticator.
func (p *DelegatedAuthenticator) AuthenticateDelegated(ctx context.Context, r *http.Request) (*billingauth.DelegatedPrincipal, error) {
	if p == nil || p.cfg.Verifier == nil || r == nil {
		return nil, billingauth.ErrUnauthenticated
	}
	cl, err := p.cfg.Verifier.VerifyRequest(r)
	if err != nil {
		return nil, err
	}
	// The self surface is subject-scoped: a token without a native user
	// subject (e.g. a service credential) has no self to act as — fail closed.
	if strings.TrimSpace(cl.UserID) == "" {
		return nil, billingauth.ErrUnauthenticated
	}
	// Admission runs BEFORE the permission resolver, so an inadmissible
	// subject never reaches the host's (typically DB-backed) grant lookup.
	if err := admit(ctx, r, cl, p.cfg.Admit); err != nil {
		return nil, err
	}
	var perms []string
	if p.cfg.Permissions != nil {
		perms, err = p.cfg.Permissions(ctx, r, cl)
		if err != nil {
			log.WithError(err).WithField("subject", cl.UserID).
				Warn("delegated permission resolver failed")
			return nil, billingauth.ErrUnauthenticated
		}
	}
	issuer := strings.TrimSpace(p.cfg.Issuer)
	if issuer == "" {
		issuer = cl.Issuer
	}
	return &billingauth.DelegatedPrincipal{
		MerchantID:    p.cfg.MerchantID,
		MerchantSlug:  p.cfg.MerchantSlug,
		SubjectID:     cl.UserID,
		Issuer:        issuer,
		Permissions:   perms,
		Email:         cl.Email,
		EmailVerified: cl.EmailVerified,
		Username:      cl.Username,
	}, nil
}
