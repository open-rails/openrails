-- billing.usage_events: append-only metered usage, partitioned by month on
-- occurred_at. Every read names a time range. The idempotency coordinate
-- (merchant, customer, currency, event_type, source, source_id) is claimed under
-- the customer spend lock by GetUsageEventByCoords over the ingest window.

-- stated_amount is the optional host cost; each meter decides whether to charge it.
-- name: InsertUsageEvent :exec
INSERT INTO billing.usage_events (
    id, merchant_id, customer_id, invoker_id, currency, resource,
    event_type, dimensions, amount, source, source_id,
    ledger_transfer_id, stated_amount, metadata, occurred_at, created_at,
    outcome, forgiven_amount, charges
) VALUES ($1, $2, $3, $4, sqlc.arg(currency), $5, $6, COALESCE(sqlc.arg(dimensions), '{}'::jsonb), $8, $9, $10, $11, sqlc.narg(stated_amount)::bigint, $12, $13, $14,
    sqlc.arg(outcome)::text, sqlc.arg(forgiven_amount)::bigint, sqlc.arg(charges)::jsonb);

-- name: GetUsageEventByCoords :one
SELECT * FROM billing.usage_events
WHERE merchant_id = $1 AND customer_id = $2 AND currency = sqlc.arg(currency)
  AND event_type = $3 AND source = $4 AND source_id = $5
  AND occurred_at >= sqlc.arg(occurred_from)::timestamptz
  AND occurred_at <= sqlc.arg(occurred_to)::timestamptz
ORDER BY occurred_at DESC
LIMIT 1;

-- name: UsageReport :many
-- One page of a customer's usage over [from, to) in one currency, grouped by
-- event_type, resource ('' when absent) or invoker: total amount descending,
-- then the key in byte order, after the group (after_amount, after_key).
SELECT g.key, g.event_count, g.total_amount
FROM (
    SELECT COALESCE(CASE sqlc.arg(group_by)::text
               WHEN 'event_type' THEN ue.event_type
               WHEN 'resource' THEN ue.resource
               WHEN 'invoker' THEN ue.invoker_id
           END, '')::text AS key,
           COUNT(*)::bigint AS event_count,
           COALESCE(SUM(ue.amount), 0)::bigint AS total_amount
    FROM billing.usage_events ue
    WHERE ue.merchant_id = sqlc.arg(merchant_id) AND ue.customer_id = sqlc.arg(customer_id)
      AND ue.currency = sqlc.arg(currency)
      AND ue.occurred_at >= sqlc.arg(from_at)::timestamptz
      AND ue.occurred_at < sqlc.arg(to_at)::timestamptz
    GROUP BY 1
) g
WHERE sqlc.narg(after_amount)::bigint IS NULL
   OR g.total_amount < sqlc.narg(after_amount)::bigint
   OR (g.total_amount = sqlc.narg(after_amount)::bigint AND g.key COLLATE "C" > sqlc.narg(after_key)::text)
ORDER BY g.total_amount DESC, g.key COLLATE "C"
LIMIT sqlc.arg(row_limit)::int;

-- name: UsageReportByMeter :many
-- One page of a customer's usage over [from, to) in one currency, grouped by
-- what each meter (and matrix cell) charged it: the key is the meter key and
-- the cell joined by U+001F. Ordered as UsageReport.
SELECT g.key, g.event_count, g.total_amount, g.quantity
FROM (
    SELECT ((c.charge ->> 'meter_key') || chr(31) || COALESCE(c.charge ->> 'cell', ''))::text AS key,
           COUNT(*)::bigint AS event_count,
           COALESCE(SUM((c.charge ->> 'amount')::bigint), 0)::bigint AS total_amount,
           COALESCE(SUM((c.charge ->> 'quantity')::bigint), 0)::bigint AS quantity
    FROM billing.usage_events ue
    CROSS JOIN LATERAL jsonb_array_elements(ue.charges) AS c(charge)
    WHERE ue.merchant_id = sqlc.arg(merchant_id) AND ue.customer_id = sqlc.arg(customer_id)
      AND ue.currency = sqlc.arg(currency)
      AND ue.occurred_at >= sqlc.arg(from_at)::timestamptz
      AND ue.occurred_at < sqlc.arg(to_at)::timestamptz
    GROUP BY 1
) g
WHERE sqlc.narg(after_amount)::bigint IS NULL
   OR g.total_amount < sqlc.narg(after_amount)::bigint
   OR (g.total_amount = sqlc.narg(after_amount)::bigint AND g.key COLLATE "C" > sqlc.narg(after_key)::text)
ORDER BY g.total_amount DESC, g.key COLLATE "C"
LIMIT sqlc.arg(row_limit)::int;

-- name: UsageReportDimensions :many
-- The summed dimensions of a report page's event types over [from, to).
SELECT ue.event_type,
       d.key::text AS key,
       COALESCE(SUM((d.value)::bigint), 0)::bigint AS total
FROM billing.usage_events ue
CROSS JOIN LATERAL jsonb_each_text(ue.dimensions) AS d
WHERE ue.merchant_id = sqlc.arg(merchant_id) AND ue.customer_id = sqlc.arg(customer_id)
  AND ue.currency = sqlc.arg(currency)
  AND ue.event_type = ANY (sqlc.arg(event_types)::text[])
  AND ue.occurred_at >= sqlc.arg(from_at)::timestamptz
  AND ue.occurred_at < sqlc.arg(to_at)::timestamptz
GROUP BY ue.event_type, d.key;

-- name: SumUsageAmountSince :one
-- accrual_rate_cap: the customer's rated usage over a lookback window, for the
-- measured accrual rate. Window-bounded, never history; served by
-- usage_events_customer_id_occurred_at_idx.
SELECT COALESCE(SUM(amount), 0)::bigint AS total_amount
FROM billing.usage_events
WHERE merchant_id = $1 AND customer_id = $2 AND currency = sqlc.arg(currency)
  AND occurred_at >= sqlc.arg(since)::timestamptz;
