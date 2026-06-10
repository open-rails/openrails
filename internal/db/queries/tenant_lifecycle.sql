-- Tenant lifecycle (#225): per-table purge/count queries for tenant
-- export + gated delete. One static query per tenant-owned table — the
-- generated replacement for the bun-era fmt.Sprintf(`billing.%s`)
-- identifier interpolation (#334's 'unsafe SQL' kill target).

-- name: CountTenantRowsProducts :one
SELECT count(*) FROM billing.products WHERE tenant_id = $1;

-- name: PurgeTenantRowsProducts :exec
DELETE FROM billing.products WHERE tenant_id = $1;

-- name: CountTenantRowsPrices :one
SELECT count(*) FROM billing.prices WHERE tenant_id = $1;

-- name: PurgeTenantRowsPrices :exec
DELETE FROM billing.prices WHERE tenant_id = $1;

-- name: CountTenantRowsCatalogDriftEvents :one
SELECT count(*) FROM billing.catalog_drift_events WHERE tenant_id = $1;

-- name: PurgeTenantRowsCatalogDriftEvents :exec
DELETE FROM billing.catalog_drift_events WHERE tenant_id = $1;

-- name: CountTenantRowsPaymentMethods :one
SELECT count(*) FROM billing.payment_methods WHERE tenant_id = $1;

-- name: PurgeTenantRowsPaymentMethods :exec
DELETE FROM billing.payment_methods WHERE tenant_id = $1;

-- name: CountTenantRowsSubscriptions :one
SELECT count(*) FROM billing.subscriptions WHERE tenant_id = $1;

-- name: PurgeTenantRowsSubscriptions :exec
DELETE FROM billing.subscriptions WHERE tenant_id = $1;

-- name: CountTenantRowsEntitlements :one
SELECT count(*) FROM billing.entitlements WHERE tenant_id = $1;

-- name: PurgeTenantRowsEntitlements :exec
DELETE FROM billing.entitlements WHERE tenant_id = $1;

-- name: CountTenantRowsPayments :one
SELECT count(*) FROM billing.payments WHERE tenant_id = $1;

-- name: PurgeTenantRowsPayments :exec
DELETE FROM billing.payments WHERE tenant_id = $1;

-- name: CountTenantRowsAdminGrants :one
SELECT count(*) FROM billing.admin_grants WHERE tenant_id = $1;

-- name: PurgeTenantRowsAdminGrants :exec
DELETE FROM billing.admin_grants WHERE tenant_id = $1;

-- name: CountTenantRowsNotificationQueue :one
SELECT count(*) FROM billing.notification_queue WHERE tenant_id = $1;

-- name: PurgeTenantRowsNotificationQueue :exec
DELETE FROM billing.notification_queue WHERE tenant_id = $1;

-- name: CountTenantRowsProcessorCustomers :one
SELECT count(*) FROM billing.processor_customers WHERE tenant_id = $1;

-- name: PurgeTenantRowsProcessorCustomers :exec
DELETE FROM billing.processor_customers WHERE tenant_id = $1;

-- name: CountTenantRowsCreditTypes :one
SELECT count(*) FROM billing.credit_types WHERE tenant_id = $1;

-- name: PurgeTenantRowsCreditTypes :exec
DELETE FROM billing.credit_types WHERE tenant_id = $1;

-- name: CountTenantRowsCreditTransactions :one
SELECT count(*) FROM billing.credit_transactions WHERE tenant_id = $1;

-- name: PurgeTenantRowsCreditTransactions :exec
DELETE FROM billing.credit_transactions WHERE tenant_id = $1;

-- name: CountTenantRowsCreditBlocks :one
SELECT count(*) FROM billing.credit_blocks WHERE tenant_id = $1;

-- name: PurgeTenantRowsCreditBlocks :exec
DELETE FROM billing.credit_blocks WHERE tenant_id = $1;

-- name: CountTenantRowsCreditBalances :one
SELECT count(*) FROM billing.credit_balances WHERE tenant_id = $1;

-- name: PurgeTenantRowsCreditBalances :exec
DELETE FROM billing.credit_balances WHERE tenant_id = $1;

-- name: CountTenantRowsCheckoutSessions :one
SELECT count(*) FROM billing.checkout_sessions WHERE tenant_id = $1;

-- name: PurgeTenantRowsCheckoutSessions :exec
DELETE FROM billing.checkout_sessions WHERE tenant_id = $1;

-- name: CountTenantRowsManualRebillAttempts :one
SELECT count(*) FROM billing.manual_rebill_attempts WHERE tenant_id = $1;

-- name: PurgeTenantRowsManualRebillAttempts :exec
DELETE FROM billing.manual_rebill_attempts WHERE tenant_id = $1;
