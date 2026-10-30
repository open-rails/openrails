-- name: InsertProviderSchedule :execrows
INSERT INTO billing.provider_schedules (merchant_id, subscription_id, customer_id, psp_id, initial_order_id, accepted_version, source_reference, provider_reference, first_due_at, cadence_hours, cadence_months, accepted_bill, created_at)
VALUES (sqlc.arg(merchant_id)::uuid, sqlc.arg(subscription_id)::uuid, sqlc.arg(customer_id)::uuid, sqlc.arg(psp_id)::uuid,
        sqlc.narg(initial_order_id)::uuid,
        (SELECT COALESCE(MAX(accepted_version), 0) + 1 FROM billing.provider_schedules WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND subscription_id = sqlc.arg(subscription_id)::uuid),
        sqlc.arg(source_reference)::text, sqlc.arg(provider_reference)::text, sqlc.arg(first_due_at)::timestamptz, sqlc.arg(cadence_hours)::integer, sqlc.arg(cadence_months)::integer,
        sqlc.arg(accepted_bill)::jsonb, sqlc.arg(now)::timestamptz)
ON CONFLICT (merchant_id, subscription_id, source_reference) DO NOTHING;

-- name: GetProviderSchedule :one
SELECT * FROM billing.provider_schedules WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND subscription_id = sqlc.arg(subscription_id)::uuid AND source_reference = sqlc.arg(source_reference)::text FOR UPDATE;

-- name: ListProviderSchedules :many
-- provider_reference is the subscription's provider id when accepted: it
-- matches by identity, so a later respelling keeps its schedules.
SELECT ps.* FROM billing.provider_schedules ps
JOIN billing.subscriptions s ON s.merchant_id = ps.merchant_id AND s.id = ps.subscription_id
WHERE ps.merchant_id = sqlc.arg(merchant_id)::uuid AND ps.subscription_id = sqlc.arg(subscription_id)::uuid
  AND billing.rail_subscription_key(s.rail, ps.provider_reference) = billing.rail_subscription_key(s.rail, sqlc.arg(provider_reference)::text)
ORDER BY ps.first_due_at ASC, ps.accepted_version ASC FOR UPDATE OF ps;

-- name: SameProviderSubscription :one
-- Whether two provider subscription ids name one subscription on the rail.
SELECT COALESCE(billing.rail_subscription_key(sqlc.arg(rail)::text, sqlc.arg(a)::text) = billing.rail_subscription_key(sqlc.arg(rail)::text, sqlc.arg(b)::text), false)::boolean AS same;

-- name: GetProviderCycleReceipt :one
SELECT * FROM billing.provider_cycle_receipts WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND psp_id = sqlc.arg(psp_id)::uuid AND transaction_id = sqlc.arg(transaction_id)::text;

-- name: InsertProviderCycleReceipt :exec
INSERT INTO billing.provider_cycle_receipts (merchant_id, psp_id, transaction_id, subscription_id, schedule_source_reference, due_at, amount, currency, occurred_at, approved)
VALUES (sqlc.arg(merchant_id)::uuid, sqlc.arg(psp_id)::uuid, sqlc.arg(transaction_id)::text, sqlc.arg(subscription_id)::uuid, sqlc.arg(schedule_source_reference)::text,
        sqlc.arg(due_at)::timestamptz, sqlc.arg(amount)::bigint, sqlc.arg(currency)::text, sqlc.arg(occurred_at)::timestamptz,
        sqlc.arg(approved)::boolean);

-- name: LatestSettledProviderCycle :one
SELECT due_at FROM billing.provider_cycle_receipts
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND subscription_id = sqlc.arg(subscription_id)::uuid AND schedule_source_reference = sqlc.arg(schedule_source_reference)::text AND approved
ORDER BY due_at DESC LIMIT 1;

-- name: SubscriptionRenewalSettledOn :one
-- A renewal bill of the subscription for the cycle due that day is complete,
-- whoever collected it (a provider receipt or OpenRails' own recovery).
SELECT EXISTS (SELECT 1 FROM billing.orders
    WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND origin = 'subscription' AND subscription_id = sqlc.arg(subscription_id)::uuid
      AND period_starts_at >= sqlc.arg(day_start)::timestamptz AND period_starts_at < sqlc.arg(day_end)::timestamptz
      AND subscription_bill_kind = 'renewal' AND status = 'complete')::boolean;
