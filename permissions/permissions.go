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
