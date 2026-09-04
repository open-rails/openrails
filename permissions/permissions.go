// Package permissions is OpenRails' public permission-string vocabulary: the
// merchant:* (seller) and customer:* (buyer/treasury) names its routes gate on.
// Embedding hosts import these instead of hardcoding literals when they stamp
// delegated principals or grant admin roles. /v1/me self-service needs no grant.
// These strings are a stable public contract; internal/controlplane references
// them so there is one source of truth.
package permissions

import "strings"

// Merchant (seller) permissions.
const (
	MerchantSettingsRead           = "merchant:settings:read"
	MerchantSettingsUpdate         = "merchant:settings:update"
	MerchantPaymentProvidersRead   = "merchant:payment-providers:read"
	MerchantPaymentProvidersUpdate = "merchant:payment-providers:update"
	MerchantCatalogRead            = "merchant:catalog:read"
	MerchantCatalogUpdate          = "merchant:catalog:update"
	MerchantCustomerSettingsRead   = "merchant:customer-settings:read"
	MerchantCustomerSettingsUpdate = "merchant:customer-settings:update"
	MerchantPaymentsRead           = "merchant:payments:read"
	MerchantPaymentsRefund         = "merchant:payments:refund"
	MerchantSubscriptionsRead      = "merchant:subscriptions:read"
	MerchantSubscriptionsUpdate    = "merchant:subscriptions:update"
	MerchantAdmissionsCreate       = "merchant:admissions:create"
	MerchantUsageRead              = "merchant:usage:read"
	MerchantRepairAlertsRead       = "merchant:repair-alerts:read"
	// MerchantMetricsRead gates the #733 analytics query surface
	// (/merchant/metrics/query + /schema) and reading the #741 dashboard
	// (a dashboard is a saved view over metrics).
	MerchantMetricsRead = "merchant:metrics:read"
	// MerchantDashboardUpdate gates writing the shared #741 dashboard layout
	// and NL widget generation (the LLM call spends real money).
	MerchantDashboardUpdate = "merchant:dashboard:update"
	// MerchantFindingsResolve gates POST /merchant/findings/{id}/resolve
	// (#692): approving a finding executes its recommendation (cancel/refund/
	// revoke/grant), so it is a distinct write grant; reads share
	// merchant:repair-alerts:read (the operator repair surface).
	MerchantFindingsResolve = "merchant:findings:resolve"
	// MerchantBillingImport gates POST /v1/import/billing (#737): a bulk
	// DeclaredBilling book import writes subscriptions/payments/payment methods
	// wholesale — owner/automation authority, not a support-role grant.
	MerchantBillingImport = "merchant:billing:import"
	// MerchantCreditsGrant gates the human-admin credit grant
	// (or#906: POST /v1/merchant/customers/{id}/credits). Money-in is the one
	// grant-shaped act whose blast radius is monetary, so it does NOT ride on
	// merchant:customer-settings:update (which the fixed #567 support role
	// holds): a support agent who can edit settings must not be able to mint
	// balance. Owner-level (merchant:*) by default, grantable narrowly by a
	// host with custom roles.
	MerchantCreditsGrant = "merchant:credits:grant"
	// MerchantCreditsRevoke controls removal of a credit grant remainder.
	// It is distinct from minting credit and from refunding a payment.
	MerchantCreditsRevoke = "merchant:credits:revoke"
	// MerchantMembersRead gates reading the merchant team roster and pending
	// invites (#760: GET /v1/merchant/team[/invites]). Deliberately the SAME
	// string as AuthKit's per-persona members:read built-in, so the OpenRails
	// route gate and AuthKit's own group-membership authorization agree exactly.
	// In the fixed merchant role catalog (#567) only the owner (merchant:*) holds
	// it, so the team surface is owner-only.
	MerchantMembersRead = "merchant:members:read"
	// MerchantMembersManage gates merchant team mutations (#760: invite, role
	// change, removal). AuthKit's per-persona members:manage built-in; owner-only
	// in the fixed #567 catalog.
	MerchantMembersManage = "merchant:members:manage"
	// MerchantCredentialsManage gates the merchant self-serve API-key surface
	// (#757: /v1/merchant/api-keys mint/list/revoke). Deliberately the SAME
	// string as AuthKit's per-persona credential-management capability
	// (`<persona>:credentials:manage`), so the OpenRails route gate and AuthKit
	// core's own mint authorization agree exactly. In the fixed merchant role
	// catalog (#567) only the owner (merchant:*) holds it.
	MerchantCredentialsManage = "merchant:credentials:manage"
)

// Platform-operator (root) permissions (#721). AuthKit's #111 rename made
// `root:` the platform-operator namespace (was `platform:`); namespace purity
// means root-persona roles may only hold `root:` perms, so openrails-saas #16's
// platform:merchants:* map 1:1 onto these. They gate the standalone
// cross-merchant directory and root-only operational overrides.
const (
	RootMerchantsRead    = "root:merchants:read"
	RootMerchantsDelete  = "root:merchants:delete"
	RootMerchantsRestore = "root:merchants:restore"
	// RootWorkerHealthRead gates the cross-merchant worker-health view, whose
	// last_error text is another merchant's verbatim job error (#SEC-22).
	RootWorkerHealthRead      = "root:worker-health:read"
	RootAdminRateLimitsUnlock = "root:admin-rate-limits:unlock"
)

// Customer (buyer/treasury) permissions.
const (
	CustomerBalanceRead            = "customer:balance:read"
	CustomerBillingUpdate          = "customer:billing:update"
	CustomerPaymentMethodsUpdate   = "customer:payment-methods:update"
	CustomerCheckoutCreate         = "customer:checkout:create"
	CustomerSpendDelegationsRead   = "customer:spend-delegations:read"
	CustomerSpendDelegationsUpdate = "customer:spend-delegations:update"
)

// Owner-role globs. The merchant/customer group `owner` auto-holds its whole
// namespace; a host grants these to a full-authority principal (merchant admin,
// or a payer acting on its own customer balance).
const (
	MerchantAll = "merchant:*"
	CustomerAll = "customer:*"
)

// Canonical role→permission preset (#913).
//
// Every AuthKit-embedding host used to hand-roll the mapping from its own
// role vocabulary onto this catalog, and both observed attempts were bugs:
// tensorhub's map emitted exactly two invented permission strings and pinned
// the WRONG merchant, 403ing its whole /customers/* subtree (th#1765);
// cozy-art wrote no bridge at all, so its entire self-service surface
// silently 404'd (ca#269). This preset is the documented DEFAULT for the
// common AuthKit-host role shapes; a host with a different vocabulary
// supplies its own mapping instead (see pkg/embedded/authkit
// WithRolePermissions) — the preset is a default, never a constraint.
//
// The tiers:
//
//   - owner, admin       → PresetOwnerPermissions: the full wildcard tier,
//     merchant:* AND customer:*. Both globs on purpose: the self-service /
//     treasury surface gates on customer:* strings, so merchant:* alone would
//     403 an owner reading its own organization's balance — the th#1765
//     failure shape.
//   - member             → PresetMemberPermissions: the customer self-service
//     set (balance read, billing update, spend-delegations read/update,
//     checkout create, payment-methods update).
//   - read-only, readonly, viewer → PresetReadOnlyPermissions: the :read
//     subset of the member set.
//
// Unknown roles contribute nothing (fail closed); matching is trimmed and
// case-insensitive. Wildcards compose with billingauth.NewDelegatedGate —
// billingauth.HasPermission expands "merchant:*"/"customer:*" prefix grants.
var (
	PresetOwnerPermissions = []string{MerchantAll, CustomerAll}

	PresetMemberPermissions = []string{
		CustomerBalanceRead,
		CustomerBillingUpdate,
		CustomerSpendDelegationsRead,
		CustomerSpendDelegationsUpdate,
		CustomerCheckoutCreate,
		CustomerPaymentMethodsUpdate,
	}

	PresetReadOnlyPermissions = []string{
		CustomerBalanceRead,
		CustomerSpendDelegationsRead,
	}
)

// ForRoles maps a principal's host roles onto the catalog via the canonical
// preset documented above: the union of each recognized role's tier, deduped,
// in tier order (owner/admin, then member, then read-only). Unrecognized
// roles are ignored; no roles (or only unrecognized ones) yields nil, which
// still authenticates for the grant-free /v1/me self-service surface but
// passes no permission gate.
func ForRoles(roles ...string) []string {
	var owner, member, readOnly bool
	for _, role := range roles {
		switch strings.ToLower(strings.TrimSpace(role)) {
		case "owner", "admin":
			owner = true
		case "member":
			member = true
		case "read-only", "readonly", "viewer":
			readOnly = true
		}
	}
	var out []string
	seen := map[string]bool{}
	add := func(perms []string) {
		for _, p := range perms {
			if !seen[p] {
				seen[p] = true
				out = append(out, p)
			}
		}
	}
	if owner {
		add(PresetOwnerPermissions)
	}
	if member {
		add(PresetMemberPermissions)
	}
	if readOnly {
		add(PresetReadOnlyPermissions)
	}
	return out
}
