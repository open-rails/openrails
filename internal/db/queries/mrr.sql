-- billing.subscription_mrr, its movements and rollups. Triggers write them;
-- these are the reads and the due work.

-- name: ListDueSubscriptionMRR :many
-- CROSS-MERCHANT: subscriptions whose MRR time alone changes at or before
-- at, in due order. The read is indexed on next_change_at, so an idle pass
-- reads no row.
SELECT m.next_change_at::timestamptz AS next_change_at, m.merchant_id, m.subscription_id
FROM billing.subscription_mrr m
WHERE m.next_change_at <= sqlc.arg(at)::timestamptz
  AND (sqlc.narg(after_at)::timestamptz IS NULL
       OR (m.next_change_at, m.merchant_id, m.subscription_id) > (sqlc.narg(after_at)::timestamptz, sqlc.narg(after_merchant_id)::uuid, sqlc.narg(after_subscription_id)::uuid))
ORDER BY m.next_change_at, m.merchant_id, m.subscription_id
LIMIT sqlc.arg(row_limit)::int;

-- name: EffectDueSubscriptionMRR :one
-- One due subscription leaves MRR at its end; false when it is no longer due.
SELECT billing.effect_due_mrr(sqlc.arg(merchant_id)::uuid, sqlc.arg(subscription_id)::uuid, sqlc.arg(at)::timestamptz)::boolean AS effected;

-- name: ListSubscriptionMRR :many
-- The MRR of a page of subscriptions, each with its scheduled change.
SELECT m.subscription_id, m.currency, m.mrr,
       e.kind AS scheduled_kind, e.mrr_delta AS scheduled_amount, e.effective_at AS scheduled_effective_at
FROM billing.subscription_mrr m
LEFT JOIN billing.mrr_scheduled e ON e.merchant_id = m.merchant_id AND e.subscription_id = m.subscription_id AND e.status = 'scheduled'
WHERE m.merchant_id = sqlc.arg(merchant_id)::uuid AND m.subscription_id = ANY(sqlc.arg(subscription_ids)::uuid[]);

-- name: SeedMRR :exec
-- Adopts the subscriptions without MRR state (an archive from before MRR) as
-- opening movements at at.
SELECT billing.seed_mrr(sqlc.arg(merchant_id)::uuid, sqlc.arg(at)::timestamptz);
