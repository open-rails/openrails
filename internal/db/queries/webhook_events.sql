-- Webhook dedup truth (#678): a row = the event's effects are durably applied.
-- Explicit merchant_id predicates on top of RLS so privileged (RLS-bypassing)
-- connections stay merchant-scoped too.

-- name: WebhookEventCompleted :one
SELECT EXISTS(
  SELECT 1 FROM openrails.webhook_events
  WHERE merchant_id = $1 AND op = $2 AND event_id = $3
) AS completed;

-- name: MarkWebhookEventCompleted :execrows
INSERT INTO openrails.webhook_events (merchant_id, op, event_id)
VALUES ($1, $2, $3)
ON CONFLICT (merchant_id, op, event_id) DO NOTHING;

-- name: DeleteCompletedWebhookEventsBefore :execrows
DELETE FROM openrails.webhook_events
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND completed_at < sqlc.arg(cutoff)::timestamptz;
