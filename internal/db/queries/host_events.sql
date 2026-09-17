-- name: ListHostEvents :many
-- A settled payment carries its payer, price and subscription from the
-- authoritative payment row so a host never re-reads the payment to route it.
SELECT h.*, p.customer_id AS payment_customer_id, p.price_id AS payment_price_id,
  p.subscription_id AS payment_subscription_id
FROM openrails.host_outbox h
LEFT JOIN openrails.payments p ON p.merchant_id = h.merchant_id AND p.id = h.payment_id
WHERE h.merchant_id = sqlc.arg(merchant_id)::uuid
  AND (sqlc.arg(event_type)::text = '' OR h.event_type = sqlc.arg(event_type)::text)
  AND (sqlc.narg(payment_id)::uuid IS NULL OR h.payment_id = sqlc.narg(payment_id)::uuid)
  AND (sqlc.arg(include_acknowledged)::boolean OR h.delivered_at IS NULL)
ORDER BY h.id
LIMIT sqlc.arg(row_limit)::int;

-- name: AcknowledgeHostEvent :execrows
UPDATE openrails.host_outbox
SET delivered_at = COALESCE(delivered_at, sqlc.arg(now)::timestamptz)
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid;
