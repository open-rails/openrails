-- name: InsertProviderSchedule :execrows
INSERT INTO billing.provider_schedules (merchant_id, subscription_id, customer_id, psp_id, initial_order_id, first_due_at, cadence_hours, accepted_bill, created_at)
VALUES (sqlc.arg(merchant_id)::uuid, sqlc.arg(subscription_id)::uuid, sqlc.arg(customer_id)::uuid, sqlc.arg(psp_id)::uuid,
        sqlc.arg(initial_order_id)::uuid, sqlc.arg(first_due_at)::timestamptz, sqlc.arg(cadence_hours)::integer,
        sqlc.arg(accepted_bill)::jsonb, sqlc.arg(now)::timestamptz)
ON CONFLICT (merchant_id, subscription_id) DO NOTHING;

-- name: GetProviderSchedule :one
SELECT * FROM billing.provider_schedules WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND subscription_id = sqlc.arg(subscription_id)::uuid FOR UPDATE;

-- name: GetProviderCycleReceipt :one
SELECT * FROM billing.provider_cycle_receipts WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND psp_id = sqlc.arg(psp_id)::uuid AND transaction_id = sqlc.arg(transaction_id)::text;

-- name: InsertProviderCycleReceipt :exec
INSERT INTO billing.provider_cycle_receipts (merchant_id, psp_id, transaction_id, subscription_id, due_at, amount, currency, occurred_at, approved)
VALUES (sqlc.arg(merchant_id)::uuid, sqlc.arg(psp_id)::uuid, sqlc.arg(transaction_id)::text, sqlc.arg(subscription_id)::uuid,
        sqlc.arg(due_at)::timestamptz, sqlc.arg(amount)::bigint, sqlc.arg(currency)::text, sqlc.arg(occurred_at)::timestamptz,
        sqlc.arg(approved)::boolean);
