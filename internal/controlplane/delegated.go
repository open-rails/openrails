package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/google/uuid"
	"github.com/open-rails/authkit/authbase"
	authcore "github.com/open-rails/authkit/core"
	authhttp "github.com/open-rails/authkit/http"

	"github.com/open-rails/openrails/pkg/billingauth"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/open-rails/openrails/pkg/merchant"
)

// ResolvedDelegated is the result of validating a browser-direct DELEGATED ACCESS
// TOKEN against the OpenRails control plane (issue #222 browser-tier foundation).
// It carries everything the self-service routes need for a human end-user acting
// on their OWN billing: the acting user (the token's `delegated_sub`) and the
// resolved OpenRails merchant the user belongs to.
type ResolvedDelegated struct {
	// Merchant is the resolved merchant's slug, sourced from the issuer registry
	// (openrails.merchants via the validated `iss`). Delegated tokens carry NO
	// merchant claims (authkit v0.23.0 issuer-only profile); the slug is
	// receiver-side directory data, identical to MerchantSlug.
	Merchant string
	// MerchantID is the resolved OpenRails merchant (#223).
	MerchantID merchant.ID
	// MerchantSlug is the resolved merchant's slug.
	MerchantSlug string
	// CustomerID is the durable OpenRails payable subject for
	// (MerchantID, DelegatedSubject).
	CustomerID uuid.UUID
	// DelegatedSubject is the acting end-user id (`delegated_sub`). This is the
	// user the self-service handlers scope every read/write to. There is NEVER a
	// normal `sub` on a delegated access token.
	//
	// SHARED-USER-NAMESPACE REQUIREMENT (issue #259): for a federated merchant whose
	// users are shared across multiple issuers (e.g. multiple host apps = distinct
	// issuers, one merchant, one user set), `delegated_sub` MUST be the merchant's
	// CANONICAL user id (the shared AuthKit subject) so a token from EITHER issuer
	// resolves to the SAME OpenRails billing account. OpenRails cannot detect a
	// divergent per-service local id, so the HOST is responsible for presenting
	// the canonical id; OpenRails uses this value verbatim as the billing account
	// key. (For merchant-admin tokens this is the ACTING ADMIN, recorded for audit.)
	DelegatedSubject string
	// Issuer is the VALIDATED token `iss`: the registered merchant issuer the merchant
	// was pinned from (every delegated token is FEDERATED merchant-signed, #259).
	// Used for audit and issuer/subject attribution (#246).
	Issuer string
	// Permissions is the token's claim, already bounded by AuthKit's verifier to the
	// signing remote-app's stored authority (#564): an over-claim rejects the token,
	// so this is a subset the signer is entitled to grant. Empty for self-service.
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
// Glob-aware, identical to every other credential type (#565): a granted token
// covers perm via AuthKit's namespace-anchored glob semantics, so a minter that
// puts `merchant:*` on a token covers `merchant:catalog:update`, while an exact
// grant still matches exactly. The minter chooses exact vs glob (a glob may
// expose more than strictly necessary — the minter's call, not a gate rule).
func (r *ResolvedDelegated) HasPermission(perm string) bool {
	for _, grant := range r.Permissions {
		if authbase.PermissionTokenCovers(grant, perm) {
			return true
		}
	}
	return false
}

// ResolvedDelegatedFromHostPrincipal validates a host-supplied IN-PROCESS
// delegated principal (billingauth.DelegatedAuthenticator output) and converts it
// to ResolvedDelegated. When OpenRails runs as a subsystem the embedding host is
// TRUSTED (in process), so its supplied permissions are authoritative — no
// allowlist (#564); merchant + subject must be explicit. Shared by the gin self
// surface (ginmw) and the gin-free merchant routes so both gate the same way.
func ResolvedDelegatedFromHostPrincipal(p *billingauth.DelegatedPrincipal) (*ResolvedDelegated, error) {
	if p == nil {
		return nil, billingauth.ErrDelegatedPrincipalInvalid
	}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	merchantID, err := merchant.ParseID(strings.TrimSpace(p.MerchantID))
	if err != nil || merchantID.IsZero() {
		return nil, billingauth.ErrDelegatedPrincipalInvalid
	}
	subject := strings.TrimSpace(p.SubjectID)
	customerID := identity.CustomerIDFromString(subject)
	if customerID.IsZero() {
		return nil, billingauth.ErrDelegatedPrincipalInvalid
	}
	perms := make([]string, 0, len(p.Permissions))
	for _, perm := range p.Permissions {
		if perm = strings.TrimSpace(perm); perm != "" {
			perms = append(perms, perm)
		}
	}
	return &ResolvedDelegated{
		Merchant:         strings.TrimSpace(p.MerchantSlug),
		MerchantID:       merchantID,
		MerchantSlug:     strings.TrimSpace(p.MerchantSlug),
		CustomerID:       customerID.UUID(),
		DelegatedSubject: subject,
		Issuer:           strings.TrimSpace(p.Issuer),
		Permissions:      perms,
		Email:            p.Email,
		EmailVerified:    p.EmailVerified,
		Username:         p.Username,
	}, nil
}

// ErrDelegatedNotConfigured indicates the control plane has no delegated-token
// verifier. That is a wiring bug (#469: the standalone always builds one), so
// this is a defensive fail-closed guard.
var ErrDelegatedNotConfigured = errors.New("controlplane: delegated access verifier not configured")

// ErrDelegatedInvalid is the sanitized error for any delegated-token rejection
// that is not specifically expiry/revocation/merchant-unresolved. It never leaks
// internal verifier detail to the response.
var ErrDelegatedInvalid = errors.New("controlplane: invalid delegated access token")

// ErrDelegatedOriginNotAllowed indicates a delegated browser request failed the
// defense-in-depth Origin check against the verified issuer's AuthKit
// remote_application. This is not API authorization: Origin is a browser header
// and can be spoofed by non-browser clients.
var ErrDelegatedOriginNotAllowed = errors.New("controlplane: delegated origin not allowed")

// DelegatedVerifier returns the control plane's delegated-access-token verifier.
// Exposed for the middleware and tests.
func (c *ControlPlane) DelegatedVerifier() *authhttp.Verifier {
	if c == nil {
		return nil
	}
	return c.delegatedVerifier
}

// newDelegatedVerifier builds the delegated-access-token Verifier for the canonical
// `openrails` audience. It applies NO OpenRails permission allowlist (#564);
// AuthKit's verifier already bounds the claim to the signer's stored authority
// (an over-claim rejects the token). VerifyDelegatedAccess enforces AuthKit's
// delegated-token profile + the no-`sub`/`delegated_sub`-present invariant.
//
// FEDERATED merchant issuers (issue #259) are loaded by reloadDelegatedIssuers
// (AddIssuer with JWKS-URL fetching), so at runtime the verifier trusts every
// registered+enabled merchant issuer — and ONLY those. OpenRails signs no delegated
// tokens itself; there is no self-issuer.
func newDelegatedVerifier(coreSvc *authcore.Service, tokenPrefix string) (*authhttp.Verifier, error) {
	if coreSvc == nil {
		return nil, ErrDelegatedNotConfigured
	}

	// The verifier starts with NO issuers. Every delegated token is FEDERATED
	// (merchant-signed); the merchant issuers + their JWKS are loaded from the registry
	// by reloadDelegatedIssuers after construction. There is no control-plane
	// self-issuer seed — OpenRails never signs delegated tokens itself.
	// #564: no OpenRails browser-safe allowlist. AuthKit bounds any permission
	// claim against the signing remote-app's live stored authority.
	v := authhttp.NewVerifier(
		authhttp.WithAPIKeyPrefix(tokenPrefix),
	)
	return v, nil
}

// ResolveDelegated validates a presented delegated access token end-to-end for
// the browser-direct self-service/admin surface:
//
//   - verifies signature/issuer/audience/expiry and requires it to be an
//     AuthKit delegated access token (`delegated_sub` present, NO `sub`),
//   - relies on AuthKit's verifier to bound the `permissions` claim to the signing
//     remote-app's stored authority (#564); an over-claim rejects the token,
//   - resolves the OpenRails merchant from the VALIDATED `iss` via the issuer
//     registry (issuer is globally unique -> pins exactly one merchant). Tokens
//     carry NO merchant claims (authkit v0.23.0 issuer-only profile); the
//     verifier rejects any token carrying `tenant` or `tenant_id`.
//
// Returns:
//   - authcore.ErrAccessTokenExpired for an expired token,
//   - ErrDelegatedIssuerUnknown when a federated token's issuer is not
//     registered+enabled for an active merchant (cross-merchant / unmapped),
//   - ErrDelegatedOriginNotAllowed when the optional browser Origin
//     defense-in-depth check fails for the verified issuer,
//   - ErrDelegatedInvalid for any other rejection (bad signature/audience/type,
//     normal-sub token, forbidden permission, forbidden merchant claim).
func (c *ControlPlane) ResolveDelegated(ctx context.Context, token string, origin string) (*ResolvedDelegated, error) {
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
	// Browser defense-in-depth only: real authorization is already the validated
	// delegated JWT shape, issuer, audience, permissions, and merchant
	// owner mapping below. Do not treat Origin as an API security boundary.
	allowed, err := c.delegatedVerifier.OriginAllowedForIssuer(ctx, issuer, origin, true)
	if err != nil || !allowed {
		return nil, ErrDelegatedOriginNotAllowed
	}

	// FEDERATED merchant-signed token (issue #259): the merchant is pinned from the
	// VALIDATED `iss` via the issuer registry. Because `issuer` is globally unique,
	// a given signing key can only ever resolve to its own merchant
	// (no-cross-merchant-forgery). An unregistered/disabled issuer fails closed.
	// The token carries no merchant claims (issuer-only profile, authkit v0.23.0):
	// the issuer registry is the SOLE source of merchant identity, so slug renames
	// never invalidate in-flight tokens.
	tid, tslug, _, _, _, err := c.merchantForIssuer(ctx, issuer)
	if err != nil {
		return nil, err
	}

	customerID, err := c.TouchCustomer(ctx, tid, issuer, subject)
	if err != nil {
		return nil, err
	}

	// principal.Permissions is ALREADY bounded by AuthKit's verifier to the signing
	// remote-app's stored authority (#564): an over-claim rejects the token before
	// we get here, so what survives is a subset the signer is entitled to grant.
	return &ResolvedDelegated{
		Merchant:             tslug,
		MerchantID:           tid,
		MerchantSlug:         tslug,
		CustomerID:           customerID,
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
