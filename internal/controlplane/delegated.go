package controlplane

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	helpersauth "github.com/open-rails/helpers/auth"

	"github.com/open-rails/authkit/dpop"
	authcore "github.com/open-rails/authkit/embedded"

	"github.com/open-rails/authkit"
	"github.com/open-rails/authkit/verify"
	"github.com/open-rails/openrails/internal/credential"

	"github.com/open-rails/openrails/internal/requestauth"
	"github.com/open-rails/openrails/pkg/billingauth"
)

// ResolvedDelegated is the result of validating a browser-direct DELEGATED ACCESS
// TOKEN against the OpenRails control plane (issue #222 browser-tier foundation).
// It carries everything the self-service routes need for a human end-user acting
// on their OWN billing: the acting user (the token's `delegated_sub`) and the
// resolved OpenRails merchant the user belongs to.
type ResolvedDelegated = credential.ResolvedDelegated

var ResolvedDelegatedFromHostPrincipal = credential.ResolvedDelegatedFromHostPrincipal

// ErrDelegatedNotConfigured indicates the control plane has no delegated-token
// verifier. That is a wiring bug (#469: the standalone always builds one), so
// this is a defensive fail-closed guard.
var ErrDelegatedNotConfigured = credential.ErrDelegatedNotConfigured

// ErrDelegatedInvalid is the sanitized error for any delegated-token rejection
// that is not specifically expiry/revocation/merchant-unresolved. It never leaks
// internal verifier detail to the response.
var ErrDelegatedInvalid = credential.ErrDelegatedInvalid

// ErrDelegatedUnavailable means sender-proof replay protection could not be
// consulted. A caller must fail closed without treating it as invalid credentials.
var ErrDelegatedUnavailable = credential.ErrDelegatedUnavailable

// DelegatedVerifier returns the control plane's delegated-access-token verifier.
// Exposed for the middleware and tests.
func (c *ControlPlane) DelegatedVerifier() *verify.Verifier {
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
func newDelegatedVerifier(coreSvc authcore.HTTPBackend, tokenPrefix string, requestURL func(*http.Request) string) (*verify.Verifier, error) {
	if coreSvc == nil {
		return nil, ErrDelegatedNotConfigured
	}

	// The verifier starts with NO issuers. Every delegated token is FEDERATED
	// (merchant-signed); the merchant issuers + their JWKS are loaded from the registry
	// by reloadDelegatedIssuers after construction. There is no control-plane
	// self-issuer seed — OpenRails never signs delegated tokens itself.
	// #564: no OpenRails browser-safe allowlist. AuthKit bounds any permission
	// claim against the signing remote-app's live stored authority.
	//
	// BND4-2: WithSSRFGuard installs AuthKit's DNS-resolving, private-range-rejecting
	// dialer for JWKS fetches. Merchant-supplied jwks_uri values pass only a syntactic
	// registration check (validateJWKSURI does NOT resolve DNS), so this fetch-time
	// guard is the required second layer against DNS-rebinding SSRF from the
	// control-plane host to cloud metadata / internal services. AuthKit's own server
	// always installs it; a verify-only embedder must opt in explicitly.
	v := verify.NewVerifier(
		verify.WithDPoP(coreSvc.ClaimDPoPProof, requestURL),
		verify.WithAPIKeyPrefix(tokenPrefix),
		verify.WithSSRFGuard(),
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
//   - authkit.ErrAccessTokenExpired for an expired token,
//   - ErrDelegatedIssuerUnknown when a federated token's issuer is not
//     registered+enabled for an active merchant (cross-merchant / unmapped),
//   - ErrDelegatedInvalid for any other rejection (bad signature/audience/type,
//     normal-sub token, forbidden permission, forbidden merchant claim).
func (c *ControlPlane) ResolveDelegated(r *http.Request) (*ResolvedDelegated, error) {
	if c == nil || c.delegatedVerifier == nil {
		return nil, ErrDelegatedNotConfigured
	}
	if r == nil {
		return nil, ErrDelegatedInvalid
	}
	ctx := r.Context()
	c.refreshIssuerRegistryIfStale()

	verified, err := requestauth.Once(ctx, c.delegatedVerifier, func() (delegatedClaims, error) {
		cl, principal, err := c.delegatedVerifier.VerifyDelegatedAccessRequest(r)
		return delegatedClaims{cl, principal}, err
	})
	cl, principal := verified.claims, verified.principal
	if err != nil {
		if errors.Is(err, dpop.ErrReplayUnavailable) {
			return nil, ErrDelegatedUnavailable
		}
		if errors.Is(err, verify.ErrSenderProofRequired) {
			return nil, errors.Join(verify.ErrSenderProofRequired, helpersauth.ErrSenderProofRequired)
		}
		// Preserve expiry so the middleware can return a precise reason; map
		// everything else to a sanitized invalid error (never leak verifier
		// internals or distinguish bad-signature from wrong-audience to clients).
		if errors.Is(err, authkit.ErrAccessTokenExpired) {
			return nil, errors.Join(authkit.ErrAccessTokenExpired, helpersauth.ErrExpired)
		}
		if errors.Is(err, authkit.ErrAccessTokenRevoked) {
			return nil, errors.Join(authkit.ErrAccessTokenRevoked, helpersauth.ErrRevoked)
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
	// Wire delegation always has a sender binding. Trusted in-process user
	// adapters are a separate interface and do not weaken this HTTP contract.
	if principal.ConfirmationCertificateSHA256 == nil && principal.ConfirmationJWKThumbprintSHA256 == nil {
		return nil, errors.Join(verify.ErrSenderProofRequired, helpersauth.ErrSenderProofRequired)
	}
	subject := strings.TrimSpace(principal.DelegatedSubject)
	if subject == "" {
		return nil, ErrDelegatedInvalid
	}

	issuer := strings.TrimSpace(principal.Issuer)

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

	class := billingauth.CredentialClassUnknown
	if raw, present := principal.Attributes[billingauth.DelegatedCredentialClassAttribute]; present {
		if err := json.Unmarshal(raw, &class); err != nil || (class != billingauth.CredentialClassUserSession && class != billingauth.CredentialClassAutomation) {
			return nil, ErrDelegatedInvalid
		}
	}
	customerID, err := c.TouchCustomer(ctx, tid, issuer, subject)
	if err != nil {
		return nil, err
	}

	// principal.Permissions is ALREADY bounded by AuthKit's verifier to the signing
	// remote-app's stored authority (#564): an over-claim rejects the token before
	// we get here, so what survives is a subset the signer is entitled to grant.
	return &ResolvedDelegated{
		CredentialClass:      class,
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

type delegatedClaims struct {
	claims    verify.Claims
	principal verify.DelegatedPrincipal
}
