-- Webhook dedup truth (#678): a row = the event's effects are durably applied.
-- The source is a PSP or a custodian; explicit merchant_id predicates scope
-- every statement.

-- name: WebhookEventCompleted :one
SELECT EXISTS(
  SELECT 1 FROM billing.webhook_events
  WHERE merchant_id = sqlc.arg(merchant_id)::uuid
    AND psp_id IS NOT DISTINCT FROM sqlc.narg(psp_id)::uuid
    AND custodian_id IS NOT DISTINCT FROM sqlc.narg(custodian_id)::uuid
    AND op = sqlc.arg(op)::text AND event_id = sqlc.arg(event_id)::text
) AS completed;

-- name: MarkWebhookEventCompleted :execrows
INSERT INTO billing.webhook_events (merchant_id, psp_id, custodian_id, op, event_id)
VALUES (sqlc.arg(merchant_id)::uuid, sqlc.narg(psp_id)::uuid, sqlc.narg(custodian_id)::uuid, sqlc.arg(op)::text, sqlc.arg(event_id)::text)
ON CONFLICT (merchant_id, psp_id, custodian_id, op, event_id) DO NOTHING;

-- or#837: batched — row_limit bounds one statement, the caller loops.
-- name: DeleteCompletedWebhookEventsBefore :execrows
DELETE FROM billing.webhook_events
WHERE ctid IN (
    SELECT we.ctid FROM billing.webhook_events we
    WHERE we.merchant_id = sqlc.arg(merchant_id)::uuid
      AND we.completed_at < sqlc.arg(cutoff)::timestamptz
    LIMIT sqlc.arg(row_limit)::int
);
