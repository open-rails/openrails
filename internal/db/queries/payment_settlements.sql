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
