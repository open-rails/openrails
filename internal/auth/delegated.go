package auth

import (
	"context"
	"net/http"
	"strings"

	"github.com/open-rails/openrails/pkg/billingauth"
	log "github.com/sirupsen/logrus"
)

// DelegatedAuthenticator is the AuthKit-verifier-backed
// billingauth.DelegatedAuthenticator: the delegated twin of Authenticator
// (#913). It verifies the host's bearer token and maps the claims onto a
// DelegatedPrincipal pinned to ONE explicit merchant — the embedding engine's
// bound merchant, supplied at construction, NEVER anything read from the
// caller's token. (tensorhub's hand-rolled bridge pinned the CALLER's org
// UUID instead, scoping every request's RLS to a nonexistent merchant —
// th#1765.)
type DelegatedAuthenticator struct {
	verifier   Verifier
	merchantID string
	// rolePermissions maps the token's host roles onto the OpenRails
	// permission catalog. Nil maps to no permissions: the principal still
	// authenticates for the grant-free /v1/me self-service surface but passes
	// no permission gate.
	rolePermissions func(roles []string) []string
}

// NewDelegatedAuthenticator builds the AuthKit-backed DelegatedAuthenticator
// around a caller-supplied verifier, merchant pin, and role→permission
// mapping. Most hosts should use the pkg/embedded/authkit constructor
// (NewVerifierDelegatedAuthenticator), which wires the issuer verifier and
// the canonical permissions.ForRoles preset.
func NewDelegatedAuthenticator(v Verifier, merchantID string, rolePermissions func(roles []string) []string) *DelegatedAuthenticator {
	return &DelegatedAuthenticator{verifier: v, merchantID: merchantID, rolePermissions: rolePermissions}
}

// AuthenticateDelegated implements billingauth.DelegatedAuthenticator.
func (p *DelegatedAuthenticator) AuthenticateDelegated(_ context.Context, r *http.Request) (*billingauth.DelegatedPrincipal, error) {
	if p == nil || p.verifier == nil {
		return nil, billingauth.ErrUnauthenticated
	}
	token := bearerToken(r.Header.Get("Authorization"))
	if token == "" {
		return nil, billingauth.ErrUnauthenticated
	}
	cl, err := p.verifier.Verify(token)
	if err != nil {
		if looksLikeJWT(token) {
			log.WithError(err).Warn("delegated jwt verification failed")
		} else {
			log.WithError(err).Debug("delegated bearer token is not a jwt; jwt verification skipped")
		}
		return nil, err
	}
	// The self surface is subject-scoped: a token without a native user
	// subject (e.g. a service credential) has no self to act as — fail closed.
	if strings.TrimSpace(cl.UserID) == "" {
		return nil, billingauth.ErrUnauthenticated
	}
	var perms []string
	if p.rolePermissions != nil {
		perms = p.rolePermissions(cl.Roles)
	}
	return &billingauth.DelegatedPrincipal{
		MerchantID:    p.merchantID,
		SubjectID:     cl.UserID,
		Issuer:        cl.Issuer,
		Permissions:   perms,
		Email:         cl.Email,
		EmailVerified: cl.EmailVerified,
		Username:      cl.Username,
	}, nil
}
