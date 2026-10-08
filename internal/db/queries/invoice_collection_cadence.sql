-- name: GetInvoiceMonthlyCollectionPeriod :one
SELECT monthly_period_started_at FROM billing.invoice_collection_cadence
WHERE merchant_id = $1;

-- name: CompleteInvoiceMonthlyCollectionPeriod :exec
INSERT INTO billing.invoice_collection_cadence (merchant_id, monthly_period_started_at, completed_at)
VALUES ($1, $2, $3)
ON CONFLICT (merchant_id) DO UPDATE
SET monthly_period_started_at = EXCLUDED.monthly_period_started_at,
    completed_at = GREATEST(billing.invoice_collection_cadence.completed_at, EXCLUDED.completed_at)
WHERE EXCLUDED.monthly_period_started_at >= billing.invoice_collection_cadence.monthly_period_started_at;

-- name: TryLockInvoiceMonthlyCollection :one
SELECT pg_try_advisory_lock(hashtextextended('openrails.invoice_monthly_collection:' || sqlc.arg(merchant_id)::uuid::text, 0))::boolean;

-- name: UnlockInvoiceMonthlyCollection :exec
SELECT pg_advisory_unlock(hashtextextended('openrails.invoice_monthly_collection:' || sqlc.arg(merchant_id)::uuid::text, 0));
