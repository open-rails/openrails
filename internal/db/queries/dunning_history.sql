-- Dunning forensics from the recorded declined attempts.
-- name: ListDunningHistoryEvents :many
SELECT
    ev.source_table::text AS source_table,
    ev.event_type::text AS event_type,
    ev.rail::text AS rail,
    ev.subscription_id,
    ev.rail_subscription_id,
    ev.rail_transaction_id,
    ev.status::text AS status,
    CASE WHEN ev.amount_micros IS NULL THEN NULL ELSE ev.amount_micros END AS amount_micros,
    ev.occurred_at
FROM (
    SELECT 'payment_attempts' AS source_table,
           'charge_failure' AS event_type,
           a.rail,
           a.subscription_id,
           NULL::text AS rail_subscription_id,
           COALESCE(a.transaction_id, '') AS rail_transaction_id,
           'failed' AS status,
           a.amount AS amount_micros,
           a.attempted_at AS occurred_at
    FROM billing.payment_attempts a
    WHERE a.merchant_id = sqlc.arg(merchant_id)::uuid
      AND a.rail = ANY(sqlc.arg(rails)::text[])
      AND a.subscription_id IS NOT NULL
      AND a.category <> 'approved'
) ev
WHERE (sqlc.narg(since)::timestamptz IS NULL OR ev.occurred_at >= sqlc.narg(since)::timestamptz)
  AND (sqlc.narg(until)::timestamptz IS NULL OR ev.occurred_at <= sqlc.narg(until)::timestamptz)
ORDER BY ev.occurred_at ASC
LIMIT sqlc.arg(limit_rows);
