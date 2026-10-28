-- name: InsertChargeAttribution :execrows
-- A declared provider charge the import could not record, once per provider
-- transaction. A replay changes nothing.
INSERT INTO billing.provider_charge_attributions (merchant_id, psp_id, transaction_id, customer_id, subscription_id, declared_at,
    status, attempts, next_attempt_at, created_at, updated_at)
VALUES (sqlc.arg(merchant_id)::uuid, sqlc.arg(psp_id)::uuid, sqlc.arg(transaction_id)::text, sqlc.arg(customer_id)::uuid,
    sqlc.arg(subscription_id)::uuid, sqlc.narg(declared_at)::timestamptz, 'pending', 0, sqlc.arg(now)::timestamptz,
    sqlc.arg(now)::timestamptz, sqlc.arg(now)::timestamptz)
ON CONFLICT (merchant_id, psp_id, transaction_id) DO NOTHING;

-- name: GetChargeAttribution :one
SELECT * FROM billing.provider_charge_attributions
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND psp_id = sqlc.arg(psp_id)::uuid AND transaction_id = sqlc.arg(transaction_id)::text;

-- name: ListChargeAttributionsByTransaction :many
-- The declared subscriptions of the provider transactions a read returned.
SELECT * FROM billing.provider_charge_attributions
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND psp_id = sqlc.arg(psp_id)::uuid
  AND transaction_id = ANY(sqlc.arg(transaction_ids)::text[])
ORDER BY transaction_id
LIMIT sqlc.arg(row_limit)::int;

-- name: ResolveChargeAttribution :execrows
-- The attributed charge is recorded as this payment, even one already given up
-- as unresolvable (a provider pull read it after all).
UPDATE billing.provider_charge_attributions
SET status = 'resolved', payment_id = sqlc.arg(payment_id)::uuid, next_attempt_at = NULL, resolved_at = sqlc.arg(now)::timestamptz,
    updated_at = sqlc.arg(now)::timestamptz
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND psp_id = sqlc.arg(psp_id)::uuid AND transaction_id = sqlc.arg(transaction_id)::text
  AND status IN ('pending', 'unresolvable');

-- name: MarkChargeAttributionUnresolvable :execrows
-- The provider's record cannot be the declared charge; no payment is recorded.
UPDATE billing.provider_charge_attributions
SET status = 'unresolvable', reason = sqlc.arg(reason)::text, attempts = attempts + 1, next_attempt_at = NULL,
    resolved_at = sqlc.arg(now)::timestamptz, updated_at = sqlc.arg(now)::timestamptz
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND psp_id = sqlc.arg(psp_id)::uuid AND transaction_id = sqlc.arg(transaction_id)::text
  AND status = 'pending';

-- name: DeferChargeAttribution :execrows
-- The provider read failed or has not indexed the transaction yet.
UPDATE billing.provider_charge_attributions
SET attempts = attempts + 1, next_attempt_at = sqlc.arg(next_attempt_at)::timestamptz, updated_at = sqlc.arg(now)::timestamptz
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND psp_id = sqlc.arg(psp_id)::uuid AND transaction_id = sqlc.arg(transaction_id)::text
  AND status = 'pending';
