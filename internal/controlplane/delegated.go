package controlplane

import (
	"context"
	"errors"
	"fmt"
	"strings"

	authcore "github.com/open-rails/authkit/core"
	authhttp "github.com/open-rails/authkit/http"

	"github.com/open-rails/openrails/pkg/tenant"
)

// ResolvedDelegated is the result of validating a browser-direct DELEGATED ACCESS
// TOKEN against the OpenRails control plane (issue #222 browser-tier foundation).
// It carries everything the self-service routes need to authorize a human
// end-user acting on their OWN billing: the acting user (the token's
// `delegated_sub`), the resolved OpenRails tenant the user belongs to, and the
// token's `openrails:self:*` permission grants.
type ResolvedDelegated struct {
	// Tenant is the AuthKit org slug from the token's `tenant` claim. It is the
	// org that administers the user's billing namespace; it maps to an OpenRails
	// tenant via the billing.tenants directory (same mapping as OATs).
	Tenant string
	// TenantID is the resolved OpenRails tenant (#223).
	TenantID tenant.ID
	// TenantSlug is the resolved tenant's slug.
	TenantSlug string
	// DelegatedSubject is the acting end-user id (`delegated_sub`). This is the
	// user the self-service handlers scope every read/write to. There is NEVER a
	// normal `sub` on a delegated access token.
	DelegatedSubject string
	// Permissions are the token's granted self-service permission strings
	// (a subset of the `openrails:self:*` catalog).
	Permissions []string
}

// HasPermission reports whether the resolved delegated token grants perm.
//
// Unlike OATs, delegated self tokens do NOT honor a broad admin grant: a browser
// token can only carry `openrails:self:*` permissions (enforced at verify time),
// so every permission check is an exact-string match against what the token
// actually carries.
func (r *ResolvedDelegated) HasPermission(perm string) bool {
	perm = strings.TrimSpace(perm)
	for _, p := range r.Permissions {
		if p == perm {
			return true
		}
	}
	return false
}

// ErrDelegatedNotConfigured indicates the deployment has no delegated-token
// verifier (verifier-only / no control plane). The self-service surface is not
// mounted in that mode, so this is a defensive guard.
var ErrDelegatedNotConfigured = errors.New("controlplane: delegated access verifier not configured")

// ErrDelegatedInvalid is the sanitized error for any delegated-token rejection
// that is not specifically expiry/revocation/tenant-unresolved. It never leaks
// internal verifier detail to the response.
var ErrDelegatedInvalid = errors.New("controlplane: invalid delegated access token")

// DelegatedVerifier returns the control plane's delegated-access-token verifier,
// or nil when the deployment is verifier-only. Exposed for the middleware and
// tests.
func (c *ControlPlane) DelegatedVerifier() *authhttp.Verifier {
	if c == nil {
		return nil
	}
	return c.delegatedVerifier
}

// newDelegatedVerifier builds a Verifier that accepts ONLY this control plane's
// own delegated access tokens: the same issuer the control plane signs as, the
// canonical `openrails` audience, the local signing public keys, and a
// permission catalog validator that rejects any permission outside the
// `openrails:self:*` self-service catalog. VerifyDelegatedAccess additionally
// enforces `typ=at+jwt` + the no-`sub`/`delegated_sub`-present invariant.
func newDelegatedVerifier(coreSvc *authcore.Service, expectedAudiences []string, orgMode, tokenPrefix string) (*authhttp.Verifier, error) {
	if coreSvc == nil {
		return nil, ErrDelegatedNotConfigured
	}
	opts := coreSvc.Options()
	issuer := strings.TrimSpace(opts.Issuer)
	if issuer == "" {
		return nil, errors.New("controlplane: control plane issuer is empty; cannot build delegated verifier")
	}

	v := authhttp.NewVerifier(
		authhttp.WithOrgMode(orgMode),
		authhttp.WithTokenPrefix(tokenPrefix),
		// Enforce that every permission on a browser token belongs to the
		// self-service catalog. A token carrying an operator/server-to-server
		// grant (or any unknown permission) is rejected by VerifyDelegatedAccess.
		authhttp.WithPermissionCatalog(func(permissions []string) error {
			if len(permissions) == 0 {
				return errors.New("delegated token carries no permissions")
			}
			for _, p := range permissions {
				if !IsSelfPermission(p) {
					return fmt.Errorf("permission %q is not an openrails:self:* self-service permission", p)
				}
			}
			return nil
		}),
	)
	if err := v.AddIssuer(issuer, expectedAudiences, authhttp.IssuerOptions{
		RawKeys: coreSvc.PublicKeysByKID(),
	}); err != nil {
		return nil, fmt.Errorf("controlplane: add delegated issuer: %w", err)
	}
	return v, nil
}

// ResolveDelegated validates a presented delegated access token end-to-end for
// the browser-direct self-service surface:
//
//   - verifies signature/issuer/audience/expiry and requires it to be a
//     delegated access token (`typ=at+jwt`, `delegated_sub` present, NO `sub`),
//   - enforces that every `permissions` entry is an `openrails:self:*` grant,
//   - requires a non-empty `tenant` claim,
//   - maps the `tenant` claim (AuthKit org slug) -> OpenRails tenant (#223) via
//     the billing.tenants directory — the SAME org->tenant mapping OATs use.
//
// Returns:
//   - authcore.ErrAccessTokenExpired for an expired token,
//   - ErrOATTenantUnresolved when the token's org maps to no active tenant
//     (cross-tenant / unmapped),
//   - ErrDelegatedInvalid for any other rejection (bad signature/audience/type,
//     normal-sub token, missing/forbidden permission, missing tenant).
func (c *ControlPlane) ResolveDelegated(ctx context.Context, token string) (*ResolvedDelegated, error) {
	if c == nil || c.delegatedVerifier == nil {
		return nil, ErrDelegatedNotConfigured
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return nil, ErrDelegatedInvalid
	}

	cl, principal, err := c.delegatedVerifier.VerifyDelegatedAccess(token)
	if err != nil {
		// Preserve expiry so the middleware can return a precise reason; map
		// everything else to a sanitized invalid error (never leak verifier
		// internals or distinguish bad-signature from wrong-audience to clients).
		if errors.Is(err, authcore.ErrAccessTokenExpired) {
			return nil, authcore.ErrAccessTokenExpired
		}
		if errors.Is(err, authcore.ErrAccessTokenRevoked) {
			return nil, authcore.ErrAccessTokenRevoked
		}
		return nil, ErrDelegatedInvalid
	}

	// Defense in depth: VerifyDelegatedAccess already guarantees the delegated
	// shape, but re-assert the load-bearing invariants explicitly so a future
	// change to the verifier cannot silently weaken the self-service boundary.
	if strings.TrimSpace(cl.UserID) != "" {
		// A normal `sub` is present: this is NOT a delegated access token.
		return nil, ErrDelegatedInvalid
	}
	subject := strings.TrimSpace(principal.DelegatedSubject)
	if subject == "" {
		return nil, ErrDelegatedInvalid
	}
	orgSlug := strings.TrimSpace(principal.Tenant)
	if orgSlug == "" {
		return nil, ErrDelegatedInvalid
	}

	tid, tslug, err := c.tenantForOrgSlug(ctx, orgSlug)
	if err != nil {
		return nil, err
	}

	return &ResolvedDelegated{
		Tenant:           orgSlug,
		TenantID:         tid,
		TenantSlug:       tslug,
		DelegatedSubject: subject,
		Permissions:      append([]string(nil), principal.Permissions...),
	}, nil
}
