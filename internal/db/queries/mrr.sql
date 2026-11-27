-- MRR facts: subscription_mrr, movements and their lines, risk cases and
-- episodes. Triggers write them; these are the reads.

-- name: ListSubscriptionMRR :many
-- A page of subscriptions' MRR rows, each with its risk case at at: open, or
-- closed at an end still ahead.
SELECT m.subscription_id, m.currency, m.mrr, m.ends_at, c.entry_reason AS risk_entry_reason, c.opened_at AS risk_since
FROM billing.subscription_mrr m
LEFT JOIN billing.mrr_risk_cases c ON c.merchant_id = m.merchant_id AND c.subscription_id = m.subscription_id
    AND (c.closed_at IS NULL OR c.closed_at > sqlc.arg(at)::timestamptz)
WHERE m.merchant_id = sqlc.arg(merchant_id)::uuid AND m.subscription_id = ANY(sqlc.arg(subscription_ids)::uuid[]);

-- name: ListScheduledMRRChanges :many
-- A page of subscriptions' scheduled changes that move MRR, priced now.
SELECT x.subscription_id::uuid AS subscription_id, x.effective_at::timestamptz AS effective_at, x.amount::bigint AS amount
FROM billing.mrr_scheduled_changes(sqlc.arg(merchant_id)::uuid) x
WHERE x.subscription_id = ANY(sqlc.arg(subscription_ids)::uuid[]);

-- name: ListMRRRiskNow :many
-- A page of subscriptions' reasons at risk at at, as state says, and since
-- when state says they hold; an open case dates them instead.
SELECT n.subscription_id::uuid AS subscription_id, n.reasons::text[] AS reasons, n.since::timestamptz AS since
FROM billing.mrr_risk_now(sqlc.arg(merchant_id)::uuid, sqlc.arg(at)::timestamptz) n
WHERE n.subscription_id = ANY(sqlc.arg(subscription_ids)::uuid[]);
