-- or#878 durable host-consumption feed. Same guarantees as
-- payment_settlement_events (0005/0010): merchant-scoped, explicitly acked,
-- pruned after delivery. A missed cut-off signal is a revenue leak and a missed
-- restore signal is an outage, so neither may be a fire-and-forget webhook.

-- name: EnqueueHostLifecycleEvent :execrows
-- Idempotent on the transition's dedupe key: re-announcing a transition is a
-- no-op, never a second instruction to the host.
INSERT INTO openrails.host_lifecycle_events
    (merchant_id, event_type, subject_type, subject_id, currency, occurred_at, data, dedupe_key)
VALUES (
    sqlc.arg(merchant_id), sqlc.arg(event_type)::text, sqlc.arg(subject_type)::text,
    sqlc.arg(subject_id), sqlc.arg(currency)::text, sqlc.arg(occurred_at)::timestamptz,
    sqlc.arg(data)::jsonb, sqlc.arg(dedupe_key)::text)
ON CONFLICT (merchant_id, dedupe_key) DO NOTHING;

-- name: ListPendingHostLifecycleEvents :many
SELECT id, merchant_id, event_type, subject_type, subject_id, currency, occurred_at, data
FROM openrails.host_lifecycle_events
WHERE merchant_id = sqlc.arg(merchant_id)
  AND delivered_at IS NULL
ORDER BY id
LIMIT sqlc.arg(row_limit);

-- name: AcknowledgeHostLifecycleEvent :execrows
UPDATE openrails.host_lifecycle_events
SET delivered_at = COALESCE(delivered_at, sqlc.arg(now)::timestamptz)
WHERE merchant_id = sqlc.arg(merchant_id)
  AND id = sqlc.arg(id);

-- name: DeleteDeliveredHostLifecycleEventsBefore :execrows
DELETE FROM openrails.host_lifecycle_events
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND delivered_at IS NOT NULL
  AND delivered_at < sqlc.arg(cutoff)::timestamptz;
