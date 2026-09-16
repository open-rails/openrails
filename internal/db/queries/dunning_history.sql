-- Dunning forensics from retained failed-payment evidence.
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
    SELECT 'payments' AS source_table,
           'charge_failure' AS event_type,
           p.rail,
           p.subscription_id,
           NULL::text AS rail_subscription_id,
           p.transaction_id AS rail_transaction_id,
           'failed' AS status,
           p.amount AS amount_micros,
           p.purchased_at AS occurred_at
    FROM openrails.payments p
    WHERE p.merchant_id = sqlc.arg(merchant_id)::uuid
      AND p.rail = ANY(sqlc.arg(rails)::text[])
      AND p.deleted_at IS NULL
      AND p.status = 'failed'
) ev
WHERE (sqlc.narg(since)::timestamptz IS NULL OR ev.occurred_at >= sqlc.narg(since)::timestamptz)
  AND (sqlc.narg(until)::timestamptz IS NULL OR ev.occurred_at <= sqlc.narg(until)::timestamptz)
ORDER BY ev.occurred_at ASC
LIMIT sqlc.arg(limit_rows);
