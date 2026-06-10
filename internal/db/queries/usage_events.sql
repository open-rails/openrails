-- billing.usage_events: append-only metered usage (#289), idempotent on
-- (tenant, payer, event_type, source, source_id).

-- name: InsertUsageEvent :exec
INSERT INTO billing.usage_events (
    id, tenant_id, tenant_subject_id, actor, resource, credit_type_id,
    event_type, dimensions, amount, source, source_id,
    credit_transaction_id, metadata, occurred_at, created_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, COALESCE(sqlc.arg(dimensions), '{}'::jsonb), $9, $10, $11, $12, $13, $14, $15);

-- name: InsertUsageEventIfAbsent :exec
INSERT INTO billing.usage_events (
    id, tenant_id, tenant_subject_id, actor, resource, credit_type_id,
    event_type, dimensions, amount, source, source_id,
    credit_transaction_id, metadata, occurred_at, created_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, COALESCE(sqlc.arg(dimensions), '{}'::jsonb), $9, $10, $11, $12, $13, $14, $15)
ON CONFLICT (tenant_id, tenant_subject_id, event_type, source, source_id) DO NOTHING;

-- name: GetUsageEventByCoords :one
SELECT * FROM billing.usage_events
WHERE tenant_id = $1 AND tenant_subject_id = $2 AND event_type = $3
  AND source = $4 AND source_id = $5
LIMIT 1;

-- name: AggregateUsageTotals :many
-- Per-event_type rollup over [from, to). credit_type_id NULL = all credit types.
SELECT event_type,
       COALESCE(SUM(amount), 0)::bigint AS total_amount,
       COUNT(*)::bigint AS event_count
FROM billing.usage_events
WHERE tenant_id = $1 AND tenant_subject_id = $2
  AND (sqlc.narg(credit_type_id)::uuid IS NULL OR credit_type_id = sqlc.narg(credit_type_id)::uuid)
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
WHERE ue.tenant_id = $1 AND ue.tenant_subject_id = $2
  AND (sqlc.narg(credit_type_id)::uuid IS NULL OR ue.credit_type_id = sqlc.narg(credit_type_id)::uuid)
  AND ue.occurred_at >= sqlc.arg(from_at)::timestamptz
  AND ue.occurred_at < sqlc.arg(to_at)::timestamptz
GROUP BY ue.event_type, d.key;

-- name: ServiceUsageRollup :many
-- Per-dimension-VALUE spend grouped by a fixed selector (#311). group_by is
-- validated against the allowlist in Go; unknown selectors group everything
-- under '' (the CASE yields NULL).
SELECT COALESCE(CASE sqlc.arg(group_by)::text
           WHEN 'resource' THEN ue.resource
           WHEN 'actor' THEN ue.actor
           WHEN 'function' THEN ue.metadata->>'function_name'
           WHEN 'tier' THEN ue.metadata->>'availability_tier'
       END, '')::text AS key,
       COUNT(*)::bigint AS event_count,
       COALESCE(SUM(ue.amount), 0)::bigint AS total_amount
FROM billing.usage_events ue
WHERE ue.tenant_id = $1 AND ue.tenant_subject_id = $2
  AND ue.occurred_at >= sqlc.arg(from_at)::timestamptz
  AND ue.occurred_at < sqlc.arg(to_at)::timestamptz
GROUP BY 1
ORDER BY total_amount DESC;

-- name: ResourceRevenueDaily :many
-- Per-day revenue for a resource across ALL payers in the tenant (#410).
-- Ledger micro-dollars -> millicents: ceil-divide by 10.
SELECT to_char(date_trunc('day', ue.occurred_at AT TIME ZONE 'UTC'), 'YYYY-MM-DD')::text AS date,
       ((COALESCE(SUM(ue.amount), 0) + 9) / 10)::bigint AS amount_millicents
FROM billing.usage_events ue
WHERE ue.tenant_id = $1 AND ue.resource = $2
  AND ue.occurred_at >= sqlc.arg(from_at)::timestamptz
  AND ue.occurred_at < sqlc.arg(to_at)::timestamptz
GROUP BY 1
ORDER BY 1;
