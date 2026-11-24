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
-- The MRR of a page of subscriptions, each with its scheduled change and its
-- open risk case.
SELECT m.subscription_id, m.currency, m.mrr, m.recorded_risk::text[] AS risk,
       e.kind AS scheduled_kind, e.mrr_delta AS scheduled_amount, e.effective_at AS scheduled_effective_at,
       c.entry_reason AS risk_entry_reason, c.opened_at AS risk_since
FROM billing.subscription_mrr m
LEFT JOIN billing.mrr_scheduled e ON e.merchant_id = m.merchant_id AND e.subscription_id = m.subscription_id AND e.status = 'scheduled'
LEFT JOIN billing.mrr_risk_cases c ON c.merchant_id = m.merchant_id AND c.subscription_id = m.subscription_id AND c.closed_at IS NULL
WHERE m.merchant_id = sqlc.arg(merchant_id)::uuid AND m.subscription_id = ANY(sqlc.arg(subscription_ids)::uuid[]);

-- name: ListMRRRiskDueMerchants :many
-- CROSS-MERCHANT: the merchants with queued risk changes, ids only. The queue
-- holds only changes not yet claimed.
SELECT DISTINCT d.merchant_id
FROM billing.mrr_risk_due d
ORDER BY d.merchant_id;

-- name: EffectDueMRRRisk :one
-- Claims up to batch of the merchant's queued risk changes no other worker
-- holds and re-evaluates their customers; returns how many it claimed.
SELECT billing.effect_due_mrr_risk(sqlc.arg(merchant_id)::uuid, sqlc.arg(batch)::int)::int AS claimed;

-- name: QueueRestoredMRRRisk :exec
-- A restore re-evaluates every customer with a subscription adding MRR: the
-- changes the source had queued stay at the source.
INSERT INTO billing.mrr_risk_due (merchant_id, customer_id, changed_at)
SELECT DISTINCT m.merchant_id, m.customer_id, sqlc.arg(at)::timestamptz
FROM billing.subscription_mrr m
WHERE m.merchant_id = sqlc.arg(merchant_id)::uuid AND m.mrr > 0;

-- name: SeedMRR :exec
-- Adopts the subscriptions without MRR state (an archive from before MRR) as
-- opening movements at at.
SELECT billing.seed_mrr(sqlc.arg(merchant_id)::uuid, sqlc.arg(at)::timestamptz);
