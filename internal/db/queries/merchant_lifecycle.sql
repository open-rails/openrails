-- Tenant lifecycle (#225): per-table purge/count queries for tenant
-- export + gated delete. One static query per tenant-owned table — the
-- generated replacement for the bun-era fmt.Sprintf(`openrails.%s`)
-- identifier interpolation (#334's 'unsafe SQL' kill target).

-- name: CountMerchantRowsProducts :one
SELECT count(*) FROM openrails.products WHERE merchant_id = $1;

-- name: PurgeMerchantRowsProducts :exec
DELETE FROM openrails.products WHERE merchant_id = $1;

-- name: CountMerchantRowsPrices :one
SELECT count(*) FROM openrails.prices WHERE merchant_id = $1;

-- name: PurgeMerchantRowsPrices :exec
DELETE FROM openrails.prices WHERE merchant_id = $1;

-- name: CountMerchantRowsCatalogDriftEvents :one
SELECT count(*) FROM openrails.catalog_drift_events WHERE merchant_id = $1;

-- name: PurgeMerchantRowsCatalogDriftEvents :exec
DELETE FROM openrails.catalog_drift_events WHERE merchant_id = $1;

-- name: CountMerchantRowsPaymentMethods :one
SELECT count(*) FROM openrails.payment_methods WHERE merchant_id = $1;

-- name: PurgeMerchantRowsPaymentMethods :exec
DELETE FROM openrails.payment_methods WHERE merchant_id = $1;

-- name: CountMerchantRowsSubscriptions :one
SELECT count(*) FROM openrails.subscriptions WHERE merchant_id = $1;

-- name: PurgeMerchantRowsSubscriptions :exec
DELETE FROM openrails.subscriptions WHERE merchant_id = $1;

-- name: CountMerchantRowsEntitlements :one
SELECT count(*) FROM openrails.entitlements WHERE merchant_id = $1;

-- name: PurgeMerchantRowsEntitlements :exec
DELETE FROM openrails.entitlements WHERE merchant_id = $1;

-- name: CountMerchantRowsPayments :one
SELECT count(*) FROM openrails.payments WHERE merchant_id = $1;

-- name: PurgeMerchantRowsPayments :exec
DELETE FROM openrails.payments WHERE merchant_id = $1;



-- name: CountMerchantRowsNotificationQueue :one
SELECT count(*) FROM openrails.notification_queue WHERE merchant_id = $1;

-- name: PurgeMerchantRowsNotificationQueue :exec
DELETE FROM openrails.notification_queue WHERE merchant_id = $1;

-- name: CountMerchantRowsProcessorCustomers :one
SELECT count(*) FROM openrails.processor_customers WHERE merchant_id = $1;

-- name: PurgeMerchantRowsProcessorCustomers :exec
DELETE FROM openrails.processor_customers WHERE merchant_id = $1;

-- name: CountMerchantRowsCheckoutSessions :one
SELECT count(*) FROM openrails.checkout_sessions WHERE merchant_id = $1;

-- name: PurgeMerchantRowsCheckoutSessions :exec
DELETE FROM openrails.checkout_sessions WHERE merchant_id = $1;

-- name: CountMerchantRowsProviderIntents :one
SELECT count(*) FROM openrails.provider_intents WHERE merchant_id = $1;

-- name: PurgeMerchantRowsProviderIntents :exec
DELETE FROM openrails.provider_intents WHERE merchant_id = $1;

-- name: CountMerchantRowsMoneyAccounts :one
SELECT count(*) FROM openrails.money_settings WHERE merchant_id = $1;

-- name: PurgeMerchantRowsMoneyAccounts :exec
DELETE FROM openrails.money_settings WHERE merchant_id = $1;
