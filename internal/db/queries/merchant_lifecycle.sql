-- Tenant lifecycle (#225): per-table purge/count queries for tenant
-- export + gated delete. One static query per tenant-owned table — the
-- generated replacement for the bun-era fmt.Sprintf(`openrails.%s`)
-- identifier interpolation (#334's 'unsafe SQL' kill target).

-- name: CountMerchantRowsProducts :one
SELECT count(*) FROM billing.products WHERE merchant_id = $1;

-- name: PurgeMerchantRowsProducts :exec
DELETE FROM billing.products WHERE merchant_id = $1;

-- name: CountMerchantRowsPrices :one
SELECT count(*) FROM billing.prices WHERE merchant_id = $1;

-- name: PurgeMerchantRowsPrices :exec
DELETE FROM billing.prices WHERE merchant_id = $1;

-- name: CountMerchantRowsCatalogDriftEvents :one
SELECT count(*) FROM billing.reconciliation_findings WHERE merchant_id = $1 AND finding_type LIKE 'catalog.%';

-- name: PurgeMerchantRowsCatalogDriftEvents :exec
DELETE FROM billing.reconciliation_findings WHERE merchant_id = $1 AND finding_type LIKE 'catalog.%';

-- name: CountMerchantRowsPaymentMethods :one
SELECT count(*) FROM billing.payment_methods WHERE merchant_id = $1;

-- name: PurgeMerchantRowsPaymentMethods :exec
DELETE FROM billing.payment_methods WHERE merchant_id = $1;

-- name: CountMerchantRowsSubscriptions :one
SELECT count(*) FROM billing.subscriptions WHERE merchant_id = $1;

-- name: PurgeMerchantRowsSubscriptions :exec
DELETE FROM billing.subscriptions WHERE merchant_id = $1;

-- name: CountMerchantRowsEntitlements :one
SELECT count(*) FROM billing.entitlements WHERE merchant_id = $1;

-- name: PurgeMerchantRowsEntitlements :exec
DELETE FROM billing.entitlements WHERE merchant_id = $1;

-- name: CountMerchantRowsPayments :one
SELECT count(*) FROM billing.payments WHERE merchant_id = $1;

-- name: PurgeMerchantRowsPayments :exec
DELETE FROM billing.payments WHERE merchant_id = $1;

-- name: CountMerchantRowsPaymentAttempts :one
SELECT count(*) FROM billing.payment_attempts WHERE merchant_id = $1;

-- name: PurgeMerchantRowsPaymentAttempts :exec
DELETE FROM billing.payment_attempts WHERE merchant_id = $1;

-- name: CountMerchantRowsRebillCycles :one
SELECT count(*) FROM billing.rebill_cycles WHERE merchant_id = $1;

-- name: PurgeMerchantRowsRebillCycles :exec
DELETE FROM billing.rebill_cycles WHERE merchant_id = $1;

-- name: CountMerchantRowsPaymentMethodUpdates :one
SELECT count(*) FROM billing.payment_method_updates WHERE merchant_id = $1;

-- name: PurgeMerchantRowsPaymentMethodUpdates :exec
DELETE FROM billing.payment_method_updates WHERE merchant_id = $1;

-- name: CountMerchantRowsNMIHistoryMonths :one
SELECT count(*) FROM billing.nmi_history_months WHERE merchant_id = $1;

-- name: PurgeMerchantRowsNMIHistoryMonths :exec
DELETE FROM billing.nmi_history_months WHERE merchant_id = $1;

-- name: CountMerchantRowsSolanaPayReceipts :one
SELECT count(*) FROM billing.solana_pay_receipts WHERE merchant_id = $1;

-- name: PurgeMerchantRowsSolanaPayReceipts :exec
DELETE FROM billing.solana_pay_receipts WHERE merchant_id = $1;

-- name: CountMerchantRowsSolanaPayReferences :one
SELECT count(*) FROM billing.solana_pay_references WHERE merchant_id = $1;

-- name: PurgeMerchantRowsSolanaPayReferences :exec
DELETE FROM billing.solana_pay_references WHERE merchant_id = $1;



-- name: CountMerchantRowsNotificationQueue :one
SELECT count(*) FROM billing.notifications WHERE merchant_id = $1;

-- name: PurgeMerchantRowsNotificationQueue :exec
DELETE FROM billing.notifications WHERE merchant_id = $1;

-- name: CountMerchantRowsRailCustomers :one
SELECT count(*) FROM billing.rail_customer_accounts WHERE merchant_id = $1;

-- name: PurgeMerchantRowsRailCustomers :exec
DELETE FROM billing.rail_customer_accounts WHERE merchant_id = $1;

-- name: CountMerchantRowsCheckoutSessions :one
SELECT count(*) FROM billing.checkout_sessions WHERE merchant_id = $1;

-- name: PurgeMerchantRowsCheckoutSessions :exec
DELETE FROM billing.checkout_sessions WHERE merchant_id = $1;

-- name: CountMerchantRowsExternalProviderMutationLogs :one
SELECT count(*) FROM billing.rail_mutation_logs WHERE merchant_id = $1;

-- name: PurgeMerchantRowsExternalProviderMutationLogs :exec
DELETE FROM billing.rail_mutation_logs WHERE merchant_id = $1;

-- name: CountMerchantRowsProviderIntents :one
SELECT count(*) FROM billing.rail_intents WHERE merchant_id = $1;

-- name: PurgeMerchantRowsProviderIntents :exec
DELETE FROM billing.rail_intents WHERE merchant_id = $1;

-- name: CountMerchantRowsMoneyAccounts :one
SELECT count(*) FROM billing.money_settings WHERE merchant_id = $1;

-- name: PurgeMerchantRowsMoneyAccounts :exec
DELETE FROM billing.money_settings WHERE merchant_id = $1;
