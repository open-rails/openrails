package billing

import "strings"

// Permission names: the merchant:* (seller) and root:* (platform operator)
// strings OpenRails routes gate on. Hosts use these
// instead of literals when they stamp delegated principals or grant roles.
// /v1/me self-service needs no grant. The strings are a stable public contract.

// Merchant (seller) permissions.
const (
	MerchantHostEventsRead         = "merchant:host-events:read"
	MerchantHostEventsAcknowledge  = "merchant:host-events:acknowledge"
	MerchantSettingsRead           = "merchant:settings:read"
	MerchantSettingsUpdate         = "merchant:settings:update"
	MerchantPSPsRead               = "merchant:psps:read"
	MerchantPSPsUpdate             = "merchant:psps:update"
	MerchantCatalogRead            = "merchant:catalog:read"
	MerchantCatalogUpdate          = "merchant:catalog:update"
	MerchantCatalogOwnRead         = "merchant:catalog:read-own"
	MerchantCatalogOwnUpdate       = "merchant:catalog:update-own"
	MerchantCustomerSettingsRead   = "merchant:customer-settings:read"
	MerchantCustomerSettingsUpdate = "merchant:customer-settings:update"
	MerchantInvoicesRead           = "merchant:invoices:read"
	MerchantInvoicesUpdate         = "merchant:invoices:update"
	MerchantInvoicesCollect        = "merchant:invoices:collect"
	// MerchantCheckoutCreate permits merchant automation to purchase for a customer.
	// It is owner-only by default and separate from customer-profile editing.
	MerchantCheckoutCreate      = "merchant:checkout:create"
	MerchantPaymentsRead        = "merchant:payments:read"
	MerchantPaymentsRefund      = "merchant:payments:refund"
	MerchantSubscriptionsRead   = "merchant:subscriptions:read"
	MerchantSubscriptionsUpdate = "merchant:subscriptions:update"
	MerchantAdmissionsCreate    = "merchant:admissions:create"
	MerchantUsageRead           = "merchant:usage:read"
	// MerchantOperationsRead reads the merchant's inbox, findings and worker
	// health.
	MerchantOperationsRead = "merchant:operations:read"
	// MerchantMetricsRead gates the #733 analytics query surface
	// (/merchant/metrics/query + /schema) and reading the #741 dashboard
	// (a dashboard is a saved view over metrics).
	MerchantMetricsRead = "merchant:metrics:read"
	// MerchantDashboardUpdate gates writing the shared #741 dashboard layout
	// and NL widget generation (the LLM call spends real money).
	MerchantDashboardUpdate = "merchant:dashboard:update"
	// MerchantFindingsResolve gates POST /merchant/findings/{id}/resolve
	// (#692): approving a finding executes its recommendation (cancel/refund/
	// revoke/grant), so it is a distinct write grant; reads need
	// merchant:operations:read.
	MerchantFindingsResolve = "merchant:findings:resolve"
	// MerchantBillingImport gates POST /v1/merchant/billing-import (#737): a bulk
	// DeclaredBilling book import writes subscriptions/payments/payment methods
	// wholesale — owner/automation authority, not a support-role grant.
	MerchantBillingImport = "merchant:billing:import"
	// MerchantBillingExport allows a complete merchant billing archive, including
	// customer/provider references and historical financial records. It is not a
	// viewer/support grant. Restoration uses MerchantBillingImport.
	MerchantBillingExport = "merchant:billing:export"
	// MerchantCreditsGrant gates minting balance and credit: the human credit
	// grant (POST /v1/merchant/customers/{id}/credits), the machine deposit
	// (POST /v1/merchant/credits/deposit) and the arrears credit line
	// (PUT /v1/merchant/credit-limit). It does NOT ride on
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
	// MerchantAccessGrantPermanent allows a manual entitlement or product-access
	// grant with no end, on top of merchant:customer-settings:update. Owner-level
	// (merchant:*) by default; a host that allows only finite manual grants
	// gives it to nobody.
	MerchantAccessGrantPermanent = "merchant:access:grant-permanent"
)

// RequiresRecentSignIn reports whether perm guards an operation that moves
// money, grants access or changes who can: a native user needs a recent
// sign-in (step-up) for it, on every route that serves it. Merchant reads,
// the dashboard layout and host-event acknowledgement do not, and neither do
// customer:* operations (a buyer acting on their own billing). Machine and
// delegated credentials carry no sign-in of their own and are exempt.
func RequiresRecentSignIn(perm string) bool {
	switch perm {
	case MerchantDashboardUpdate, MerchantHostEventsAcknowledge:
		return false
	}
	rest, ok := strings.CutPrefix(perm, "merchant:")
	return ok && !strings.HasSuffix(rest, ":read") && !strings.HasSuffix(rest, ":read-own")
}

// Platform-operator (root) permissions (#721). AuthKit's #111 rename made
// `root:` the platform-operator namespace (was `platform:`); namespace purity
// means root-persona roles may only hold `root:` perms, so the hosted product's
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

// MerchantAll is the merchant owner's glob: the merchant group's owner holds
// the whole namespace.
const MerchantAll = "merchant:*"
