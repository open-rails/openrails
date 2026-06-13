-- Tenant lifecycle (#225): per-table purge/count queries for tenant
-- export + gated delete. One static query per tenant-owned table — the
-- generated replacement for the bun-era fmt.Sprintf(`openrails.%s`)
-- identifier interpolation (#334's 'unsafe SQL' kill target).

-- name: CountTenantRowsProducts :one
SELECT count(*) FROM openrails.products WHERE tenant_id = $1;

-- name: PurgeTenantRowsProducts :exec
DELETE FROM openrails.products WHERE tenant_id = $1;

-- name: CountTenantRowsPrices :one
SELECT count(*) FROM openrails.prices WHERE tenant_id = $1;

-- name: PurgeTenantRowsPrices :exec
DELETE FROM openrails.prices WHERE tenant_id = $1;

-- name: CountTenantRowsCatalogDriftEvents :one
SELECT count(*) FROM openrails.catalog_drift_events WHERE tenant_id = $1;

-- name: PurgeTenantRowsCatalogDriftEvents :exec
DELETE FROM openrails.catalog_drift_events WHERE tenant_id = $1;

-- name: CountTenantRowsPaymentMethods :one
SELECT count(*) FROM openrails.payment_methods WHERE tenant_id = $1;

-- name: PurgeTenantRowsPaymentMethods :exec
DELETE FROM openrails.payment_methods WHERE tenant_id = $1;

-- name: CountTenantRowsSubscriptions :one
SELECT count(*) FROM openrails.subscriptions WHERE tenant_id = $1;

-- name: PurgeTenantRowsSubscriptions :exec
DELETE FROM openrails.subscriptions WHERE tenant_id = $1;

-- name: CountTenantRowsEntitlements :one
SELECT count(*) FROM openrails.entitlements WHERE tenant_id = $1;

-- name: PurgeTenantRowsEntitlements :exec
DELETE FROM openrails.entitlements WHERE tenant_id = $1;

-- name: CountTenantRowsPayments :one
SELECT count(*) FROM openrails.payments WHERE tenant_id = $1;

-- name: PurgeTenantRowsPayments :exec
DELETE FROM openrails.payments WHERE tenant_id = $1;

-- name: CountTenantRowsAdminGrants :one
SELECT count(*) FROM openrails.admin_grants WHERE tenant_id = $1;

-- name: PurgeTenantRowsAdminGrants :exec
DELETE FROM openrails.admin_grants WHERE tenant_id = $1;

-- name: CountTenantRowsNotificationQueue :one
SELECT count(*) FROM openrails.notification_queue WHERE tenant_id = $1;

-- name: PurgeTenantRowsNotificationQueue :exec
DELETE FROM openrails.notification_queue WHERE tenant_id = $1;

-- name: CountTenantRowsProcessorCustomers :one
SELECT count(*) FROM openrails.processor_customers WHERE tenant_id = $1;

-- name: PurgeTenantRowsProcessorCustomers :exec
DELETE FROM openrails.processor_customers WHERE tenant_id = $1;

-- name: CountTenantRowsCheckoutSessions :one
SELECT count(*) FROM openrails.checkout_sessions WHERE tenant_id = $1;

-- name: PurgeTenantRowsCheckoutSessions :exec
DELETE FROM openrails.checkout_sessions WHERE tenant_id = $1;

-- name: CountTenantRowsProviderIntents :one
SELECT count(*) FROM openrails.provider_intents WHERE tenant_id = $1;

-- name: PurgeTenantRowsProviderIntents :exec
DELETE FROM openrails.provider_intents WHERE tenant_id = $1;

-- name: CountTenantRowsMoneyAccounts :one
SELECT count(*) FROM openrails.money_accounts WHERE tenant_id = $1;

-- name: PurgeTenantRowsMoneyAccounts :exec
DELETE FROM openrails.money_accounts WHERE tenant_id = $1;

-- name: CountTenantRowsMoneyBalances :one
SELECT count(*) FROM openrails.money_balances WHERE tenant_id = $1;

-- name: PurgeTenantRowsMoneyBalances :exec
DELETE FROM openrails.money_balances WHERE tenant_id = $1;

-- name: CountTenantRowsMoneyBlocks :one
SELECT count(*) FROM openrails.money_blocks WHERE tenant_id = $1;

-- name: PurgeTenantRowsMoneyBlocks :exec
DELETE FROM openrails.money_blocks WHERE tenant_id = $1;

-- name: CountTenantRowsMoneyTransactions :one
SELECT count(*) FROM openrails.money_transactions WHERE tenant_id = $1;

-- name: PurgeTenantRowsMoneyTransactions :exec
DELETE FROM openrails.money_transactions WHERE tenant_id = $1;

-- name: CountTenantRowsMoneyWindows :one
SELECT count(*) FROM openrails.money_windows WHERE tenant_id = $1;

-- name: PurgeTenantRowsMoneyWindows :exec
DELETE FROM openrails.money_windows WHERE tenant_id = $1;

-- name: CountTenantRowsMoneySpendLimits :one
SELECT count(*) FROM openrails.money_spend_limits WHERE tenant_id = $1;

-- name: PurgeTenantRowsMoneySpendLimits :exec
DELETE FROM openrails.money_spend_limits WHERE tenant_id = $1;
