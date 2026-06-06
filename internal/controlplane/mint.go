package controlplane

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	authhttp "github.com/open-rails/authkit/http"
)

// Delegated mint defaults/caps for the browser tier (issue #222). A host backend
// requests a TTL; the control plane clamps it into [1s, MintMaxTTL] and applies
// MintDefaultTTL when none is requested. Short lifetimes keep a leaked browser
// token cheap to contain.
const (
	// MintDefaultTTL is applied when the caller does not request a TTL.
	MintDefaultTTL = 5 * time.Minute
	// MintMaxTTL caps a minted delegated access token's lifetime.
	MintMaxTTL = 15 * time.Minute
)

// ErrMintNotConfigured indicates the control plane has no active signer/issuer to
// mint with. Defensive: New always wires these when the control plane is enabled.
var ErrMintNotConfigured = errors.New("controlplane: delegated mint not configured")

// ErrMintNoSubject indicates the mint request omitted the delegated subject.
var ErrMintNoSubject = errors.New("controlplane: delegated_sub is required")

// ErrMintNoPermissions indicates the mint request carried no permissions. A
// delegated access token with zero permissions is useless (and rejected by the
// verifier), so we reject it at mint time with a clear error.
var ErrMintNoPermissions = errors.New("controlplane: at least one permission is required")

// ErrMintForbiddenPermission indicates a requested permission is not an
// `openrails:self:*` self-service permission. Browser tokens may ONLY carry
// self-service grants; minting any operator/server-to-server permission is
// refused so a host backend can never mint itself a privileged browser token.
type ErrMintForbiddenPermission struct {
	Permission string
}

func (e *ErrMintForbiddenPermission) Error() string {
	return fmt.Sprintf("controlplane: permission %q is not an openrails:self:* self-service permission", e.Permission)
}

// MintDelegatedParams describes a browser-tier delegated access token to mint on
// behalf of a tenant's host backend.
type MintDelegatedParams struct {
	// Tenant is the AuthKit tenant slug the token is scoped to. The caller MUST pass
	// the CALLING service token's tenant (resolved server-side) — never a value from the
	// request body — so cross-tenant minting is impossible.
	Tenant string
	// DelegatedSubject is the end-user id the browser token will act as
	// (`delegated_sub`).
	DelegatedSubject string
	// Permissions are the requested `openrails:self:*` grants. Every entry must be
	// in the self catalog (IsSelfPermission) or the mint is refused.
	Permissions []string
	// TTL is the requested lifetime. It is clamped into [1s, MintMaxTTL]; a
	// non-positive TTL becomes MintDefaultTTL.
	TTL time.Duration
}

// MintedDelegatedToken is the result of a successful mint.
type MintedDelegatedToken struct {
	// Token is the signed delegated access token (JWT). The browser presents it as
	// `Authorization: Bearer <token>` against /v1/self/*.
	Token string
	// ExpiresAt is the token's `exp` instant (UTC).
	ExpiresAt time.Time
}

// MintDelegatedAccessToken mints a short-lived, user-scoped delegated access
// token for the browser-direct self-service surface (issue #222 browser tier).
//
// DEPRECATED (issue #259): this central mint path exists only for the dual-trust
// migration window. The federated model has each tenant host SELF-SIGN its own
// aud=openrails tokens (verified via the tenant's registered JWKS), removing the
// frontend -> host -> OpenRails mint round-trip. New hosts should register an
// issuer (POST /v1/service/tenant/issuers) and mint locally; this method + the
// `openrails:self:mint` service token permission are retired once all tenants self-sign.
//
// It signs with the control plane's OWN active signing key — the SAME key the
// `/v1/self` delegated verifier trusts — using AuthKit's delegated access token
// mint helper to stamp the canonical JOSE/profile header and claims (`iss`,
// `aud`=control-plane audiences, `tenant`, `delegated_sub`, `permissions`;
// NEVER `sub`). The result is therefore accepted by the existing
// DelegatedSelfRequired middleware for the same tenant + delegated_sub.
//
// Authorization to CALL this is the caller's responsibility: it must be gated by
// an service token holding PermSelfMint, and the caller MUST pass the service token's resolved tenant
// as p.Tenant (never a body-supplied tenant). This method enforces the remaining
// invariants: a non-empty subject, at least one permission, and that EVERY
// requested permission is an `openrails:self:*` self-service permission.
func (c *ControlPlane) MintDelegatedAccessToken(ctx context.Context, p MintDelegatedParams) (*MintedDelegatedToken, error) {
	if c == nil || c.signer == nil || strings.TrimSpace(c.issuer) == "" {
		return nil, ErrMintNotConfigured
	}

	tenant := strings.TrimSpace(p.Tenant)
	if tenant == "" {
		// The caller failed to bind the service token's tenant; refuse rather than mint an
		// untenanted token.
		return nil, ErrServiceTokenTenantUnresolved
	}

	subject := strings.TrimSpace(p.DelegatedSubject)
	if subject == "" {
		return nil, ErrMintNoSubject
	}

	// Validate + normalize the requested permissions against the self catalog.
	perms := make([]string, 0, len(p.Permissions))
	for _, raw := range p.Permissions {
		perm := strings.TrimSpace(raw)
		if perm == "" {
			continue
		}
		if !IsSelfPermission(perm) {
			return nil, &ErrMintForbiddenPermission{Permission: perm}
		}
		perms = append(perms, perm)
	}
	if len(perms) == 0 {
		return nil, ErrMintNoPermissions
	}

	ttl := clampMintTTL(p.TTL)

	token, err := authhttp.MintDelegatedAccessToken(ctx, c.signer, authhttp.DelegatedAccessParams{
		Issuer:           c.issuer,
		Audiences:        append([]string(nil), c.delegatedAudiences...),
		Tenant:           tenant,
		DelegatedSubject: subject,
		Permissions:      perms,
		TTL:              ttl,
	})
	if err != nil {
		return nil, fmt.Errorf("controlplane: mint delegated access token: %w", err)
	}

	return &MintedDelegatedToken{
		Token:     token,
		ExpiresAt: time.Now().Add(ttl).UTC(),
	}, nil
}

// clampMintTTL applies the default + cap to a requested TTL.
func clampMintTTL(requested time.Duration) time.Duration {
	if requested <= 0 {
		return MintDefaultTTL
	}
	if requested > MintMaxTTL {
		return MintMaxTTL
	}
	return requested
}
