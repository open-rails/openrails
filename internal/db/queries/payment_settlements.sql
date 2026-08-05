-- name: ListPendingPaymentSettlements :many
SELECT id, merchant_id, payment_id, amount, currency, settled_at
  FROM openrails.payment_settlement_events
 WHERE merchant_id = sqlc.arg(merchant_id)
   AND delivered_at IS NULL
 ORDER BY id
 LIMIT sqlc.arg(row_limit);

-- name: AcknowledgePaymentSettlement :execrows
UPDATE openrails.payment_settlement_events
   SET delivered_at = COALESCE(delivered_at, now())
 WHERE merchant_id = sqlc.arg(merchant_id)
   AND id = sqlc.arg(id);

-- or#837: batched — row_limit bounds one statement, the caller loops.
-- name: DeleteDeliveredPaymentSettlementsBefore :execrows
DELETE FROM openrails.payment_settlement_events
 WHERE ctid IN (
    SELECT pse.ctid FROM openrails.payment_settlement_events pse
     WHERE pse.merchant_id = sqlc.arg(merchant_id)::uuid
       AND pse.delivered_at IS NOT NULL
       AND pse.delivered_at < sqlc.arg(cutoff)::timestamptz
     LIMIT sqlc.arg(row_limit)::int
);
