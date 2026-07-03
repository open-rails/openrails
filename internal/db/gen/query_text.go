package gen

// QueryText exposes sqlc-generated query SQL by query name for the plan/perf
// harness (internal/db/querytest). The values ARE the generated constants in
// this package, so the SQL the harness EXPLAINs can never drift from what the
// application actually runs — the whole point of the perf gate. The consts are
// package-private to `gen`, so this registry MUST live here.
//
// Keys are the sqlc `-- name:` identifiers. Membership is the set of growable-
// table hot access patterns the perf harness measures; add an entry when a query
// joins the gate. PK / unique point lookups are O(1) and are deliberately absent.
var QueryText = map[string]string{
	// Entitlements (read-hot: access checks).
	"EntitlementExistsActive":            entitlementExistsActive,            // idx_entitlements_customer_active_window
	"ListActiveEntitlementNamesMerchant": listActiveEntitlementNamesMerchant, // idx_entitlements_customer_active_window

	// Subscriptions by customer (read-hot: access/billing).
	"GetActiveSubscriptionByCustomerAt": getActiveSubscriptionByCustomerAt, // idx_subscriptions_customer_active_created (migration 042)

	// Customer resolution + stored instruments.
	"LookupCustomerIDsBySubjects":  lookupCustomerIDsBySubjects,  // uq_customers_merchant_subject
	"ListPaymentMethodsByCustomer": listPaymentMethodsByCustomer, // idx_payment_methods_customer

	// Admission / ledger / credit-lot path (read-hot: service admit, spend).
	"GetAdmissionCapacity":    getAdmissionCapacity,    // uq_ledger_accounts_customer + money_settings_pkey
	"ListGrantsByCustomer":    listGrantsByCustomer,    // idx_grants_customer
	"ListSpendableCreditLots": listSpendableCreditLots, // idx_grants_customer_kind + idx_grants_supersedes

	// Usage rollup over the highest-volume table.
	"AggregateUsageTotals": aggregateUsageTotals, // ix_usage_events_payer_type_time

	// Latest charge for a subscription (idx_payments_subscription_id + cheap top-N
	// Sort over a subscription's bounded charge set; see migration 042 NOTE).
	"GetLatestChargeBySubscriptionID": getLatestChargeBySubscriptionID,
}
