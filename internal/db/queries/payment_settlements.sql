-- Batched: row_limit bounds one statement, the caller loops.
-- name: DeletePaymentSettlementsBefore :execrows
DELETE FROM billing.host_outbox
 WHERE ctid IN (
    SELECT pse.ctid FROM billing.host_outbox pse
     WHERE pse.merchant_id = sqlc.arg(merchant_id)::uuid
       AND pse.event_type = 'payment.settled'
       AND pse.recorded_at < sqlc.arg(cutoff)::timestamptz
     LIMIT sqlc.arg(row_limit)::int
);
