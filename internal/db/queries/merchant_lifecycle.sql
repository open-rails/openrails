-- Merchant lifecycle: per-table count/purge queries for merchant export and
-- gated delete, one static query per merchant-owned table.

-- name: CountMerchantRowsMandates :one
SELECT count(*) FROM billing.mandates WHERE merchant_id = $1;

-- name: PurgeMerchantRowsMandates :exec
DELETE FROM billing.mandates WHERE merchant_id = $1;

-- name: CountMerchantRowsPaymentMethods :one
SELECT count(*) FROM billing.payment_methods WHERE merchant_id = $1;

-- name: PurgeMerchantRowsPaymentMethods :exec
DELETE FROM billing.payment_methods WHERE merchant_id = $1;

-- name: CountMerchantRowsSubscriptions :one
SELECT count(*) FROM billing.subscriptions WHERE merchant_id = $1;

-- name: PurgeMerchantRowsSubscriptions :exec
DELETE FROM billing.subscriptions WHERE merchant_id = $1;

-- name: CountMerchantRowsProductAccess :one
SELECT count(*) FROM billing.product_access WHERE merchant_id = $1;

-- name: PurgeMerchantRowsProductAccess :exec
DELETE FROM billing.product_access WHERE merchant_id = $1;

-- name: CountMerchantRowsDisputes :one
SELECT count(*) FROM billing.disputes WHERE merchant_id = $1;

-- name: PurgeMerchantRowsDisputes :exec
DELETE FROM billing.disputes WHERE merchant_id = $1;

-- name: CountMerchantRowsFraudReports :one
SELECT count(*) FROM billing.fraud_reports WHERE merchant_id = $1;

-- name: PurgeMerchantRowsFraudReports :exec
DELETE FROM billing.fraud_reports WHERE merchant_id = $1;

-- name: CountMerchantRowsNetworkActivity :one
SELECT count(*) FROM billing.network_activity_daily WHERE merchant_id = $1;

-- name: PurgeMerchantRowsNetworkActivity :exec
DELETE FROM billing.network_activity_daily WHERE merchant_id = $1;

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

-- name: CountMerchantRowsPaymentMethodVersions :one
SELECT count(*) FROM billing.payment_method_versions WHERE merchant_id = $1;

-- name: PurgeMerchantRowsPaymentMethodVersions :exec
DELETE FROM billing.payment_method_versions WHERE merchant_id = $1;

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



-- name: CountMerchantRowsRailCustomers :one
SELECT count(*) FROM billing.psp_customers WHERE merchant_id = $1;

-- name: PurgeMerchantRowsRailCustomers :exec
DELETE FROM billing.psp_customers WHERE merchant_id = $1;

-- name: CountMerchantRowsOrderAttempts :one
SELECT count(*) FROM billing.order_attempts WHERE merchant_id = $1;

-- name: PurgeMerchantRowsOrderAttempts :exec
DELETE FROM billing.order_attempts WHERE merchant_id = $1;

-- name: CountMerchantRowsOperationLog :one
SELECT count(*) FROM billing.operation_log WHERE merchant_id = $1;

-- name: PurgeMerchantRowsOperationLog :exec
DELETE FROM billing.operation_log WHERE merchant_id = $1;

-- name: CountMerchantRowsProviderIntents :one
SELECT count(*) FROM billing.provider_intents WHERE merchant_id = $1;

-- name: PurgeMerchantRowsProviderIntents :exec
DELETE FROM billing.provider_intents WHERE merchant_id = $1;

-- name: CountMerchantRowsMoneyAccounts :one
SELECT count(*) FROM billing.money_settings WHERE merchant_id = $1;

-- name: PurgeMerchantRowsMoneyAccounts :exec
DELETE FROM billing.money_settings WHERE merchant_id = $1;
