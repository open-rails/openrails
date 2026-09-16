-- name: ListHostEvents :many
SELECT * FROM openrails.host_outbox
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND (sqlc.arg(event_type)::text = '' OR event_type = sqlc.arg(event_type)::text)
  AND (sqlc.narg(payment_id)::uuid IS NULL OR payment_id = sqlc.narg(payment_id)::uuid)
  AND (sqlc.arg(include_acknowledged)::boolean OR delivered_at IS NULL)
ORDER BY id
LIMIT sqlc.arg(row_limit)::int;

-- name: AcknowledgeHostEvent :execrows
UPDATE openrails.host_outbox
SET delivered_at = COALESCE(delivered_at, sqlc.arg(now)::timestamptz)
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid;
