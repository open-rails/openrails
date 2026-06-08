package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
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
	// Tenant is the AuthKit tenant slug from the token's `tenant` claim. It is the
	// tenant that administers the user's billing namespace; it maps to an OpenRails
	// tenant via the billing.tenants directory (same mapping as service tokens).
	Tenant string
	// TenantID is the resolved OpenRails tenant (#223).
	TenantID tenant.ID
	// TenantSlug is the resolved tenant's slug.
	TenantSlug string
	// TenantSubjectID is the durable OpenRails payable subject for
	// (TenantID, Issuer, DelegatedSubject).
	TenantSubjectID uuid.UUID
	// DelegatedSubject is the acting end-user id (`delegated_sub`). This is the
	// user the self-service handlers scope every read/write to. There is NEVER a
	// normal `sub` on a delegated access token.
	//
	// SHARED-USER-NAMESPACE REQUIREMENT (issue #259): for a federated tenant whose
	// users are shared across multiple issuers (e.g. multiple host apps = distinct
	// issuers, one tenant, one user set), `delegated_sub` MUST be the tenant's
	// CANONICAL user id (the shared AuthKit subject) so a token from EITHER issuer
	// resolves to the SAME OpenRails billing account. OpenRails cannot detect a
	// divergent per-service local id, so the HOST is responsible for presenting
	// the canonical id; OpenRails uses this value verbatim as the billing account
	// key. (For tenant-admin tokens this is the ACTING ADMIN, recorded for audit.)
	DelegatedSubject string
	// Issuer is the VALIDATED token `iss`: the registered tenant issuer the tenant
	// was pinned from (every delegated token is FEDERATED tenant-signed, #259).
	// Used for audit and as the `iss:sub` invoker form (#246).
	Issuer string
	// Permissions are the token's granted permission strings — a subset of the
	// `openrails:self:*` (self) and `openrails:tenant:*` (tenant-admin) catalogs
	// (#259). Enforced at verify time; never service/operator grants.
	Permissions []string
	// Email/Username are optional non-authoritative identity fields from delegated
	// token attributes. They are for hosted checkout/contact metadata only;
	// authorization remains delegated_sub + permissions.
	Email         string
	EmailVerified bool
	Username      string
	// Solana wallet attributes are issuer-verified facts copied from the host
	// AuthKit account into the delegated token. Self-service wallet-link writes
	// trust these claims, never browser-supplied wallet addresses.
	SolanaAddress        string
	SolanaPrimarySNSName string
	SolanaVerifiedAt     string
}

// HasPermission reports whether the resolved delegated token grants perm.
//
// Unlike service tokens, delegated self tokens do NOT honor a broad admin grant: a browser
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
// FEDERATED tenant issuers (issue #259) are loaded by reloadDelegatedIssuers
// (AddIssuer with JWKS-URL fetching), so at runtime the verifier trusts every
// registered+enabled tenant issuer — and ONLY those. OpenRails signs no delegated
// tokens itself; there is no self-issuer.
func newDelegatedVerifier(coreSvc *authcore.Service, tokenPrefix string) (*authhttp.Verifier, error) {
	if coreSvc == nil {
		return nil, ErrDelegatedNotConfigured
	}

	// The verifier starts with NO issuers. Every delegated token is FEDERATED
	// (tenant-signed); the tenant issuers + their JWKS are loaded from the registry
	// by reloadDelegatedIssuers after construction. There is no control-plane
	// self-issuer seed — OpenRails never signs delegated tokens itself.
	v := authhttp.NewVerifier(
		authhttp.WithServiceTokenPrefix(tokenPrefix),
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
//     window) from the `tenant` claim (AuthKit tenant slug) via billing.tenants.
//
// Returns:
//   - authcore.ErrAccessTokenExpired for an expired token,
//   - ErrServiceTokenTenantUnresolved when a self-issued token's tenant maps to no active
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
	authKitTenantSlug := strings.TrimSpace(principal.Tenant)

	// FEDERATED tenant-signed token (issue #259): the tenant is pinned from the
	// VALIDATED `iss` via the issuer registry. Because `issuer` is globally unique,
	// a given signing key can only ever resolve to its own tenant
	// (no-cross-tenant-forgery). An unregistered/disabled issuer fails closed.
	// There is no longer a self-issued path: every delegated token is tenant-signed
	// and verified through the tenant's registered JWKS.
	tid, tslug, err := c.tenantForIssuer(ctx, issuer)
	if err != nil {
		return nil, err
	}
	// If the token ALSO carries a `tenant` claim, it MUST agree with the issuer's
	// OpenRails tenant/resource slug.
	if authKitTenantSlug != "" && !strings.EqualFold(authKitTenantSlug, tslug) {
		return nil, ErrDelegatedInvalid
	}
	tenLbl := tslug

	tenantSubjectID, err := c.TouchTenantSubject(ctx, tid, issuer, subject)
	if err != nil {
		return nil, err
	}

	return &ResolvedDelegated{
		Tenant:               tenLbl,
		TenantID:             tid,
		TenantSlug:           tslug,
		TenantSubjectID:      tenantSubjectID,
		DelegatedSubject:     subject,
		Issuer:               issuer,
		Permissions:          append([]string(nil), principal.Permissions...),
		Email:                delegatedStringAttribute(principal.Attributes, "email"),
		EmailVerified:        delegatedBoolAttribute(principal.Attributes, "email_verified"),
		Username:             delegatedStringAttribute(principal.Attributes, "username"),
		SolanaAddress:        delegatedStringAttribute(principal.Attributes, "solana_address"),
		SolanaPrimarySNSName: delegatedStringAttribute(principal.Attributes, "solana_primary_sns_name"),
		SolanaVerifiedAt:     delegatedStringAttribute(principal.Attributes, "solana_verified_at"),
	}, nil
}

func delegatedStringAttribute(attrs map[string]json.RawMessage, key string) string {
	raw, ok := attrs[key]
	if !ok || len(raw) == 0 {
		return ""
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return ""
	}
	return strings.TrimSpace(value)
}

func delegatedBoolAttribute(attrs map[string]json.RawMessage, key string) bool {
	raw, ok := attrs[key]
	if !ok || len(raw) == 0 {
		return false
	}
	var value bool
	if err := json.Unmarshal(raw, &value); err != nil {
		return false
	}
	return value
}
