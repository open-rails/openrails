-- or#878 durable host-consumption feed. Same guarantees as
-- host_outbox (0005/0010): merchant-scoped, explicitly acked,
-- pruned after delivery. A missed cut-off signal is a revenue leak and a missed
-- restore signal is an outage, so neither may be a fire-and-forget webhook.

-- name: EnqueueHostLifecycleEvent :execrows
-- Idempotent on the transition's dedupe key: re-announcing a transition is a
-- no-op, never a second instruction to the host.
INSERT INTO openrails.host_outbox
    (merchant_id, event_type, subject_type, subject_id, currency, occurred_at, data, dedupe_key)
VALUES (
    sqlc.arg(merchant_id), sqlc.arg(event_type)::text, sqlc.arg(subject_type)::text,
    sqlc.arg(subject_id), sqlc.arg(currency)::text, sqlc.arg(occurred_at)::timestamptz,
    sqlc.arg(data)::jsonb, sqlc.arg(dedupe_key)::text)
ON CONFLICT (merchant_id, dedupe_key) DO NOTHING;

-- name: ListPendingHostLifecycleEvents :many
SELECT id, merchant_id, event_type, subject_type, subject_id, currency, occurred_at, data
FROM openrails.host_outbox
WHERE merchant_id = sqlc.arg(merchant_id)
  AND event_type <> 'payment.settled' AND delivered_at IS NULL
ORDER BY id
LIMIT sqlc.arg(row_limit);

-- name: AcknowledgeHostLifecycleEvent :execrows
UPDATE openrails.host_outbox
SET delivered_at = COALESCE(delivered_at, sqlc.arg(now)::timestamptz)
WHERE merchant_id = sqlc.arg(merchant_id)
  AND event_type <> 'payment.settled' AND id = sqlc.arg(id);

-- or#837: batched — row_limit bounds one statement, the caller loops.
-- name: DeleteDeliveredHostLifecycleEventsBefore :execrows
DELETE FROM openrails.host_outbox
WHERE ctid IN (
    SELECT hle.ctid FROM openrails.host_outbox hle
    WHERE hle.merchant_id = sqlc.arg(merchant_id)::uuid
      AND hle.event_type <> 'payment.settled' AND hle.delivered_at IS NOT NULL
      AND hle.delivered_at < sqlc.arg(cutoff)::timestamptz
    LIMIT sqlc.arg(row_limit)::int
);
