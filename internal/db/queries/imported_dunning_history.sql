-- openrails.imported_dunning_history — append-only legacy dunning forensics
-- (#735; doujins #387 import target). Display/report evidence only.

-- name: InsertImportedDunningHistory :execrows
INSERT INTO openrails.imported_dunning_history (
    id, merchant_id, subscription_id, customer_id, event_type, rail,
    occurred_at, source, detail
) VALUES (
    sqlc.arg(id), sqlc.arg(merchant_id), sqlc.narg(subscription_id),
    sqlc.narg(customer_id), sqlc.arg(event_type), sqlc.arg(rail),
    sqlc.arg(occurred_at), sqlc.arg(source),
    sqlc.narg(detail)
)
ON CONFLICT (id) DO NOTHING;

-- Dunning-forensics history feed (#735): imported legacy rows ∪ failed
-- payments, merchant-scoped, oldest first. Structured so #733's
-- subscription_status_transitions can join as another UNION branch.
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
    SELECT 'imported_dunning_history' AS source_table,
           h.event_type,
           h.rail,
           h.subscription_id,
           h.detail->>'rail_subscription_id' AS rail_subscription_id,
           h.detail->>'rail_transaction_id' AS rail_transaction_id,
           COALESCE(h.detail->>'status', '') AS status,
           (h.detail->>'amount_micros')::bigint AS amount_micros,
           h.occurred_at
    FROM openrails.imported_dunning_history h
    WHERE h.merchant_id = sqlc.arg(merchant_id)::uuid
      AND h.rail = ANY(sqlc.arg(rails)::text[])
    UNION ALL
    SELECT 'payments',
           'charge_failure',
           p.rail,
           p.subscription_id,
           NULL,
           p.transaction_id,
           'failed',
           p.amount,
           p.purchased_at
    FROM openrails.payments p
    WHERE p.merchant_id = sqlc.arg(merchant_id)::uuid
      AND p.rail = ANY(sqlc.arg(rails)::text[])
      AND p.status = 'failed'
) ev
WHERE (sqlc.narg(since)::timestamptz IS NULL OR ev.occurred_at >= sqlc.narg(since)::timestamptz)
  AND (sqlc.narg(until)::timestamptz IS NULL OR ev.occurred_at <= sqlc.narg(until)::timestamptz)
ORDER BY ev.occurred_at ASC
LIMIT sqlc.arg(limit_rows);
