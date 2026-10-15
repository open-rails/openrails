-- name: GetAccessTenure :many
-- One row per requested key. A key's valid-time history intersects every
-- product-access window; range_agg unions overlaps before time is counted.
-- Historical keys remain countable after retirement. Future access, removed
-- windows and other customers or merchants cannot contribute.
WITH windows AS (
    SELECT pe.entitlement,
           greatest(pa.starts_at, pe.added_at) AS starts_at,
           least(pa.ends_at, pa.revoked_at, pe.removed_at, sqlc.arg(at_time)::timestamptz) AS ends_at
    FROM billing.product_access pa
    JOIN billing.product_entitlements pe ON pe.merchant_id = pa.merchant_id AND pe.product_id = pa.product_id
    WHERE pa.merchant_id = sqlc.arg(merchant_id)::uuid
      AND pa.customer_id = sqlc.arg(customer_id)::uuid
      AND pa.deleted_at IS NULL
      AND pe.entitlement = ANY(sqlc.arg(entitlements)::text[])
), merged AS (
    SELECT entitlement, range_agg(tstzrange(starts_at, ends_at, '[)')) AS held
    FROM windows WHERE starts_at < ends_at
    GROUP BY entitlement
), durations AS (
    SELECT merged.entitlement,
           floor(sum(extract(epoch FROM (upper(w.span) - lower(w.span)))))::bigint AS seconds
    FROM merged CROSS JOIN LATERAL unnest(merged.held) AS w(span)
    GROUP BY merged.entitlement
)
SELECT requested.entitlement::text AS entitlement, coalesce(durations.seconds, 0)::bigint AS seconds
FROM unnest(sqlc.arg(entitlements)::text[]) AS requested(entitlement)
LEFT JOIN durations ON durations.entitlement = requested.entitlement
ORDER BY requested.entitlement
LIMIT cardinality(sqlc.arg(entitlements)::text[]);
