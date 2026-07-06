// Package permissions is OpenRails' public permission-string vocabulary: the
// merchant:* (seller) and customer:* (buyer/treasury) names its routes gate on.
// Embedding hosts import these instead of hardcoding literals when they stamp
// delegated principals or grant admin roles. /v1/me self-service needs no grant.
// These strings are a stable public contract; internal/controlplane references
// them so there is one source of truth.
package permissions

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
// platform:merchants:* map 1:1 onto these. They gate the cross-merchant
// /v1/platform/merchants directory (standalone only).
const (
	RootMerchantsRead    = "root:merchants:read"
	RootMerchantsDelete  = "root:merchants:delete"
	RootMerchantsRestore = "root:merchants:restore"
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
