-- name: ListPendingPaymentSettlements :many
SELECT id, merchant_id, payment_id, amount, currency, settled_at
  FROM openrails.payment_settlement_events
 WHERE delivered_at IS NULL
 ORDER BY id
 LIMIT $1;

-- name: AcknowledgePaymentSettlement :exec
UPDATE openrails.payment_settlement_events
   SET delivered_at = COALESCE(delivered_at, now())
 WHERE id = $1;
