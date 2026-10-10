// Package credential contains billing identities resolved by a host verifier.
package credential

import (
	"github.com/google/uuid"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/billingauth"
	"strings"
)

type ResolvedServiceCredential struct {
	// KeyID is the API key's id and Issuer the deployment holding it: the
	// actor a staff verdict names.
	KeyID  string
	Issuer string
	// OwnerGroupID is the internal id of the merchant permission group the
	// credential is nested under: the caller's authority anchor.
	OwnerGroupID string
	// OwnerGroupRef is that merchant group's resource ref (the merchant slug),
	// presentation/audit only.
	OwnerGroupRef string
	// MerchantID is the OpenRails merchant the credential administers.
	MerchantID billing.MerchantID
	// MerchantSlug is the resolved merchant's slug.
	MerchantSlug string
	// Permissions is the credential's granted OpenRails permission set
	// (`merchant:` permissions resolved from the group role).
	Permissions []string
}

// HasPermission reports whether the credential grants perm, by AuthKit's
// namespace-anchored globs (`merchant:*` covers `merchant:catalog:update`).
func (r *ResolvedServiceCredential) HasPermission(perm string) bool {
	for _, grant := range r.Permissions {
		if permissionMatches(grant, perm) {
			return true
		}
	}
	return false
}

// AllowsCustomer reports whether this credential may act for a payable
// subject: a merchant credential is merchant-wide. Customer spend delegation
// is a budget checked at admission, not a scope here.
func (r *ResolvedServiceCredential) AllowsCustomer(subject uuid.UUID) bool {
	return r != nil && !r.MerchantID.IsZero() && subject != uuid.Nil
}

type ResolvedDelegated struct {
	CredentialClass billingauth.CredentialClass
	// Merchant is the resolved merchant's slug, the same as MerchantSlug.
	Merchant string
	// MerchantID is the resolved OpenRails merchant.
	MerchantID billing.MerchantID
	// MerchantSlug is the resolved merchant's slug.
	MerchantSlug string
	// CustomerID is the durable OpenRails payable subject for
	// (MerchantID, DelegatedSubject).
	CustomerID uuid.UUID
	// DelegatedSubject is the access token's `sub`, the issuer's user, used
	// verbatim as the customer id: issuers sharing a merchant's users must
	// present the same canonical user id.
	DelegatedSubject string
	// Issuer is the verified token's `iss`, for audit and attribution.
	Issuer string
	// Invoker is an invoker spending CustomerID's balance without being the
	// customer; a signed access token has nothing to carry it in.
	Invoker string
	// Permissions is the token's grant; a customer token carries none.
	Permissions []string
	// Email and Username are the token's contact claims, never authority.
	Email                string
	EmailVerified        bool
	Username             string
	SolanaAddress        string
	SolanaPrimarySNSName string
	SolanaVerifiedAt     string
}

// HasPermission reports whether the token grants perm, by AuthKit's
// namespace-anchored globs (`merchant:*` covers `merchant:catalog:update`).
func (r *ResolvedDelegated) HasPermission(perm string) bool {
	for _, grant := range r.Permissions {
		if permissionMatches(grant, perm) {
			return true
		}
	}
	return false
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
