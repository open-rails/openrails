-- billing.usage_events: append-only metered usage, partitioned by month on
-- occurred_at. Every read names a time range. The idempotency coordinate
-- (merchant, customer, currency, event_type, source, source_id) is claimed under
-- the customer spend lock by GetUsageEventByCoords over the ingest window.

-- pricing_authority is explicit: host is already final money (including capture zero); catalog is an unpriced meter input.
-- name: InsertUsageEvent :exec
INSERT INTO billing.usage_events (
    id, merchant_id, customer_id, invoker_id, currency, resource,
    event_type, dimensions, amount, source, source_id,
    ledger_transfer_id, pricing_authority, metadata, occurred_at, created_at,
    outcome, forgiven_amount
) VALUES ($1, $2, $3, $4, sqlc.arg(currency), $5, $6, COALESCE(sqlc.arg(dimensions), '{}'::jsonb), $8, $9, $10, $11, sqlc.arg(pricing_authority), $12, $13, $14,
    sqlc.arg(outcome)::text, sqlc.arg(forgiven_amount)::bigint);

-- name: GetUsageEventByCoords :one
SELECT * FROM billing.usage_events
WHERE merchant_id = $1 AND customer_id = $2 AND currency = sqlc.arg(currency)
  AND event_type = $3 AND source = $4 AND source_id = $5
  AND occurred_at >= sqlc.arg(occurred_from)::timestamptz
  AND occurred_at <= sqlc.arg(occurred_to)::timestamptz
ORDER BY occurred_at DESC
LIMIT 1;

-- name: AggregateUsageTotals :many
-- Per-event_type rollup over [from, to) in one currency.
SELECT event_type,
       COALESCE(SUM(amount), 0)::bigint AS total_amount,
       COUNT(*)::bigint AS event_count
FROM billing.usage_events
WHERE merchant_id = $1 AND customer_id = $2 AND currency = sqlc.arg(currency)
  AND occurred_at >= sqlc.arg(from_at)::timestamptz
  AND occurred_at < sqlc.arg(to_at)::timestamptz
GROUP BY event_type;

-- name: AggregateUsageDimensions :many
-- Summed per-dimension counts per event_type over [from, to).
SELECT ue.event_type,
       d.key::text AS key,
       COALESCE(SUM((d.value)::bigint), 0)::bigint AS total
FROM billing.usage_events ue
CROSS JOIN LATERAL jsonb_each_text(ue.dimensions) AS d
WHERE ue.merchant_id = $1 AND ue.customer_id = $2
  AND ue.currency = sqlc.arg(currency)
  AND ue.occurred_at >= sqlc.arg(from_at)::timestamptz
  AND ue.occurred_at < sqlc.arg(to_at)::timestamptz
GROUP BY ue.event_type, d.key;

-- name: ServiceUsageRollup :many
-- Per-dimension-value spend grouped by a fixed selector. group_by is validated
-- against the allowlist in Go; unknown selectors group everything under ''.
SELECT COALESCE(CASE sqlc.arg(group_by)::text
           WHEN 'resource' THEN ue.resource
           WHEN 'invoker' THEN ue.invoker_id
       END, '')::text AS key,
       ue.currency,
       COUNT(*)::bigint AS event_count,
       COALESCE(SUM(ue.amount), 0)::bigint AS total_amount
FROM billing.usage_events ue
WHERE ue.merchant_id = $1 AND ue.customer_id = $2
  AND ue.currency = sqlc.arg(currency)
  AND ue.occurred_at >= sqlc.arg(from_at)::timestamptz
  AND ue.occurred_at < sqlc.arg(to_at)::timestamptz
GROUP BY 1, 2
ORDER BY total_amount DESC;

-- name: ResourceRevenueDaily :many
-- Per-day revenue for a resource across the merchant's customers. ue.amount is
-- already in the currency's ledger scale: no conversion.
SELECT to_char(date_trunc('day', ue.occurred_at AT TIME ZONE 'UTC'), 'YYYY-MM-DD')::text AS date,
       ue.currency,
       COALESCE(SUM(ue.amount), 0)::bigint AS amount
FROM billing.usage_events ue
WHERE ue.merchant_id = $1 AND ue.resource = $2 AND ue.currency = sqlc.arg(currency)
  AND ue.occurred_at >= sqlc.arg(from_at)::timestamptz
  AND ue.occurred_at < sqlc.arg(to_at)::timestamptz
GROUP BY 1, 2
ORDER BY 1;

-- name: SumUsageAmountSince :one
-- accrual_rate_cap: the customer's rated usage over a lookback window, for the
-- measured accrual rate. Window-bounded, never history; served by
-- usage_events_customer_id_occurred_at_idx.
SELECT COALESCE(SUM(amount), 0)::bigint AS total_amount
FROM billing.usage_events
WHERE merchant_id = $1 AND customer_id = $2 AND currency = sqlc.arg(currency)
  AND occurred_at >= sqlc.arg(since)::timestamptz;
