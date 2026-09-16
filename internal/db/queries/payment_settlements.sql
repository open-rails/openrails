-- name: ListPendingPaymentSettlements :many
SELECT id, merchant_id, payment_id, amount, currency, occurred_at AS settled_at
  FROM openrails.host_outbox
 WHERE merchant_id = sqlc.arg(merchant_id)
   AND event_type = 'payment.settled' AND delivered_at IS NULL
 ORDER BY id
 LIMIT sqlc.arg(row_limit);

-- name: AcknowledgePaymentSettlement :execrows
UPDATE openrails.host_outbox
   SET delivered_at = COALESCE(delivered_at, now())
 WHERE merchant_id = sqlc.arg(merchant_id)
   AND event_type = 'payment.settled' AND id = sqlc.arg(id);

-- or#837: batched — row_limit bounds one statement, the caller loops.
-- name: DeleteDeliveredPaymentSettlementsBefore :execrows
DELETE FROM openrails.host_outbox
 WHERE ctid IN (
    SELECT pse.ctid FROM openrails.host_outbox pse
     WHERE pse.merchant_id = sqlc.arg(merchant_id)::uuid
       AND pse.event_type = 'payment.settled' AND pse.delivered_at IS NOT NULL
       AND pse.delivered_at < sqlc.arg(cutoff)::timestamptz
     LIMIT sqlc.arg(row_limit)::int
);
