// Package credential contains billing identities resolved by a host verifier.
package credential

import (
	"github.com/google/uuid"
	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/pkg/billingauth"
	"github.com/open-rails/openrails/pkg/merchant"
	"strings"
)

type ResolvedServiceCredential struct {
	// OwnerGroupID is the internal id of the merchant permission-group the
	// credential is nested under (#567) — the caller's authority anchor.
	OwnerGroupID string
	// OwnerGroupRef is that merchant group's resource ref (the merchant slug),
	// presentation/audit only.
	OwnerGroupRef string
	// MerchantID is the OpenRails merchant (#480) the credential administers.
	MerchantID merchant.ID
	// MerchantSlug is the resolved merchant's slug.
	MerchantSlug string
	// Permissions is the credential's granted OpenRails permission set
	// (`merchant:` permissions resolved from the group role).
	Permissions []string
}

// HasPermission reports whether the resolved credential grants perm. Glob-aware,
// identical to every other credential type (#565): a granted token covers perm
// via AuthKit's namespace-anchored glob semantics (`merchant:*` covers
// `merchant:catalog:update`; an exact grant still matches exactly).
func (r *ResolvedServiceCredential) HasPermission(perm string) bool {
	for _, grant := range r.Permissions {
		if permissionMatches(grant, perm) {
			return true
		}
	}
	return false
}

// AllowsCustomer reports whether this credential may act for a payable subject
// inside its resolved merchant. #569 (hard cut): merchant credentials are
// merchant-wide — there is no "merchant key scoped to one customer" concept — so
// a resolved merchant credential may act for any payable subject within its
// merchant. Customer spend-delegation policy is a separate OpenRails budget
// constraint checked during admission; it is not an AuthKit resource scope.
func (r *ResolvedServiceCredential) AllowsCustomer(subject uuid.UUID) bool {
	return r != nil && !r.MerchantID.IsZero() && subject != uuid.Nil
}

type ResolvedDelegated struct {
	CredentialClass billingauth.CredentialClass
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
	// Invoker is the opaque host-owned spend principal this credential acts as
	// under CustomerID's account (or#930). Non-empty means INVOKER-SCOPED: the
	// caller spends the payer's money without being the payer, so it may read
	// its own spend windows and nothing else. Only the host-principal seam
	// (billingauth.DelegatedPrincipal) sets it — the invoker string is host-owned
	// and opaque, so a signed delegated token has nothing to carry it in.
	Invoker string
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
		if permissionMatches(grant, perm) {
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
// surface and the merchant routes (internal/http/middleware + routes) so both gate the same way.
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
		CredentialClass:  p.CredentialClass,
		Merchant:         strings.TrimSpace(p.MerchantSlug),
		MerchantID:       merchantID,
		MerchantSlug:     strings.TrimSpace(p.MerchantSlug),
		CustomerID:       customerID.UUID(),
		DelegatedSubject: subject,
		Issuer:           strings.TrimSpace(p.Issuer),
		Invoker:          strings.TrimSpace(p.Invoker),
		Permissions:      perms,
		Email:            p.Email,
		EmailVerified:    p.EmailVerified,
		Username:         p.Username,
	}, nil
}

func LooksLikeJWT(token string) bool {
	token = strings.TrimSpace(token)
	if token == "" {
		return false
	}
	return strings.Count(token, ".") == 2
}

// permissionMatches preserves the namespace-anchored wire credential policy.
// Unlike a trusted host grant, a bare wildcard never authorizes a wire token.
func permissionMatches(grant, permission string) bool {
	g := strings.Split(strings.TrimSpace(grant), ":")
	c := strings.Split(strings.TrimSpace(permission), ":")
	if g[0] == "" || g[0] == "*" {
		return false
	}
	if len(g) == 2 && g[1] == "*" {
		return c[0] == g[0]
	}
	if len(g) != len(c) {
		return false
	}
	for i := range g {
		if g[i] != c[i] && (i == 0 || g[i] != "*") {
			return false
		}
	}
	return true
}
