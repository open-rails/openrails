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
	//
	// SHARED-USER-NAMESPACE REQUIREMENT (issue #259): for a federated tenant whose
	// users are shared across multiple issuers (e.g. doujins + hentai0 = distinct
	// issuers, one tenant, one user set), `delegated_sub` MUST be the tenant's
	// CANONICAL user id (the shared AuthKit subject) so a token from EITHER issuer
	// resolves to the SAME OpenRails billing account. OpenRails cannot detect a
	// divergent per-service local id, so the HOST is responsible for presenting
	// the canonical id; OpenRails uses this value verbatim as the billing account
	// key. (For tenant-admin tokens this is the ACTING ADMIN, recorded for audit.)
	DelegatedSubject string
	// Issuer is the VALIDATED token `iss`. For a FEDERATED tenant-signed token
	// (#259) this is the registered tenant issuer the tenant was pinned from; for
	// a self-issued token (migration window) it is the control plane's own issuer.
	// Used for audit and as the `iss:sub` invoker form (#246).
	Issuer string
	// Permissions are the token's granted permission strings — a subset of the
	// `openrails:self:*` (self) and `openrails:tenant:*` (tenant-admin) catalogs
	// (#259). Enforced at verify time; never service/operator grants.
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

// newDelegatedVerifier builds the delegated-access-token Verifier seeded with
// this control plane's OWN self-issuer (the #222 path): the same issuer the
// control plane signs as, the canonical `openrails` audience, the local signing
// public keys, and a permission-catalog validator that rejects any permission
// outside the {self-service, tenant-admin} catalog (issue #259).
// VerifyDelegatedAccess additionally enforces AuthKit's delegated access token
// profile + the no-`sub`/`delegated_sub`-present invariant.
//
// FEDERATED tenant issuers (issue #259) are added on top of this seed by
// reloadDelegatedIssuers (AddIssuer with JWKS-URL fetching), so at runtime the
// verifier trusts BOTH the self-issuer (deprecated, migration window) AND every
// registered+enabled tenant issuer. The self-issuer seed is retired once all
// tenants self-sign.
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
		// {self-service, tenant-admin} catalog (issue #259). A token carrying an
		// operator/server-to-server grant (or any unknown permission) is rejected
		// by VerifyDelegatedAccess. This is the SOLE permission gate for
		// tenant-signed tokens, so it is load-bearing: exact-match, no wildcard.
		authhttp.WithPermissionCatalog(func(permissions []string) error {
			if len(permissions) == 0 {
				return errors.New("delegated token carries no permissions")
			}
			for _, p := range permissions {
				if !IsDelegatedPermission(p) {
					return fmt.Errorf("permission %q is not an openrails:self:* or openrails:tenant:* delegated permission", p)
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
//   - verifies signature/issuer/audience/expiry and requires it to be an
//     AuthKit delegated access token (`delegated_sub` present, NO `sub`),
//   - enforces that every `permissions` entry is in the {self, tenant-admin}
//     catalog (#259), never a service/operator grant,
//   - resolves the OpenRails tenant: for a FEDERATED tenant-signed token (#259)
//     from the VALIDATED `iss` via the issuer registry (issuer is globally
//     unique -> pins exactly one tenant); for a SELF-ISSUED token (migration
//     window) from the `tenant` claim (AuthKit org slug) via billing.tenants.
//
// Returns:
//   - authcore.ErrAccessTokenExpired for an expired token,
//   - ErrOATTenantUnresolved when a self-issued token's org maps to no active
//     tenant,
//   - ErrDelegatedIssuerUnknown when a federated token's issuer is not
//     registered+enabled for an active tenant (cross-tenant / unmapped),
//   - ErrDelegatedInvalid for any other rejection (bad signature/audience/type,
//     normal-sub token, missing/forbidden permission, tenant-claim mismatch).
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

	issuer := strings.TrimSpace(principal.Issuer)
	orgSlug := strings.TrimSpace(principal.Tenant)

	var (
		tid    tenant.ID
		tslug  string
		tenLbl string // value surfaced as ResolvedDelegated.Tenant (org-slug-ish)
	)

	switch {
	case issuer != "" && issuer == strings.TrimSpace(c.issuer):
		// SELF-ISSUED token (issue #222, migration window / dual-trust): OpenRails
		// itself signed it. The issuer carries no tenant binding of its own, so the
		// tenant is resolved from the `tenant` claim (AuthKit org slug) exactly as
		// before. This path is removed once all tenants self-sign (#259 task 11).
		if orgSlug == "" {
			return nil, ErrDelegatedInvalid
		}
		var err error
		tid, tslug, err = c.tenantForOrgSlug(ctx, orgSlug)
		if err != nil {
			return nil, err
		}
		tenLbl = orgSlug

	default:
		// FEDERATED tenant-signed token (issue #259): the tenant is pinned from the
		// VALIDATED `iss` via the issuer registry. Because `issuer` is globally
		// unique, a given signing key can only ever resolve to its own tenant
		// (no-cross-tenant-forgery). An unregistered/disabled issuer fails closed.
		var err error
		tid, tslug, err = c.tenantForIssuer(ctx, issuer)
		if err != nil {
			return nil, err
		}
		// If the token ALSO carries a `tenant` claim, it MUST agree with the
		// issuer's tenant — a tenant-signed token can never name a different
		// tenant than the one its issuer is pinned to.
		if orgSlug != "" {
			claimTID, _, cerr := c.tenantForOrgSlug(ctx, orgSlug)
			if cerr != nil || claimTID != tid {
				return nil, ErrDelegatedInvalid
			}
		}
		tenLbl = tslug
	}

	return &ResolvedDelegated{
		Tenant:           tenLbl,
		TenantID:         tid,
		TenantSlug:       tslug,
		DelegatedSubject: subject,
		Issuer:           issuer,
		Permissions:      append([]string(nil), principal.Permissions...),
	}, nil
}
