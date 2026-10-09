-- billing.price_migrations: one move of subscribers to another price.
-- Matched and skipped are facts of creation; the per-status counts come from
-- the migration's scheduled changes on read.

-- name: CreatePriceMigration :one
INSERT INTO billing.price_migrations (
    merchant_id, from_price_id, from_product_id, from_price_key, to_price_id, effective_at,
    fallback_policy, subscriptions_matched, subscriptions_skipped
) VALUES (
    sqlc.arg(merchant_id)::uuid, sqlc.narg(from_price_id)::uuid, sqlc.narg(from_product_id)::uuid,
    sqlc.narg(from_price_key)::text, sqlc.arg(to_price_id)::uuid, sqlc.arg(effective_at)::timestamptz,
    sqlc.arg(fallback_policy)::text, sqlc.arg(subscriptions_matched)::int, 0
)
RETURNING *;

-- name: SetPriceMigrationSkipped :execrows
UPDATE billing.price_migrations SET subscriptions_skipped = sqlc.arg(subscriptions_skipped)::int
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid;

-- name: CancelPriceMigration :execrows
UPDATE billing.price_migrations SET canceled_at = sqlc.arg(now)::timestamptz
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid AND canceled_at IS NULL;

-- name: GetPriceMigration :one
SELECT sqlc.embed(m), product.key AS product_key, c.scheduled, c.applied, c.canceled, c.blocked
FROM billing.price_migrations m
LEFT JOIN billing.products product ON product.merchant_id = m.merchant_id AND product.id = m.from_product_id
CROSS JOIN LATERAL (
    SELECT count(*) FILTER (WHERE sc.status = 'scheduled') AS scheduled,
           count(*) FILTER (WHERE sc.status = 'applied') AS applied,
           count(*) FILTER (WHERE sc.status = 'canceled') AS canceled,
           count(*) FILTER (WHERE sc.status = 'blocked') AS blocked
    FROM billing.scheduled_changes sc
    WHERE sc.merchant_id = m.merchant_id AND sc.price_migration_id = m.id
) c
WHERE m.merchant_id = sqlc.arg(merchant_id)::uuid AND m.id = sqlc.arg(id)::uuid;

-- name: ListPriceMigrationsByIDs :many
SELECT sqlc.embed(m), product.key AS product_key, c.scheduled, c.applied, c.canceled, c.blocked
FROM billing.price_migrations m
LEFT JOIN billing.products product ON product.merchant_id = m.merchant_id AND product.id = m.from_product_id
CROSS JOIN LATERAL (
    SELECT count(*) FILTER (WHERE sc.status = 'scheduled') AS scheduled,
           count(*) FILTER (WHERE sc.status = 'applied') AS applied,
           count(*) FILTER (WHERE sc.status = 'canceled') AS canceled,
           count(*) FILTER (WHERE sc.status = 'blocked') AS blocked
    FROM billing.scheduled_changes sc
    WHERE sc.merchant_id = m.merchant_id AND sc.price_migration_id = m.id
) c
WHERE m.merchant_id = sqlc.arg(merchant_id)::uuid AND m.id = ANY(sqlc.arg(ids)::uuid[])
ORDER BY m.created_at DESC, m.id DESC;

-- name: ListPriceMigrationsPage :many
SELECT sqlc.embed(m), product.key AS product_key, c.scheduled, c.applied, c.canceled, c.blocked
FROM billing.price_migrations m
LEFT JOIN billing.products product ON product.merchant_id = m.merchant_id AND product.id = m.from_product_id
CROSS JOIN LATERAL (
    SELECT count(*) FILTER (WHERE sc.status = 'scheduled') AS scheduled,
           count(*) FILTER (WHERE sc.status = 'applied') AS applied,
           count(*) FILTER (WHERE sc.status = 'canceled') AS canceled,
           count(*) FILTER (WHERE sc.status = 'blocked') AS blocked
    FROM billing.scheduled_changes sc
    WHERE sc.merchant_id = m.merchant_id AND sc.price_migration_id = m.id
) c
WHERE m.merchant_id = sqlc.arg(merchant_id)::uuid
  AND (sqlc.narg(from_product_id)::uuid IS NULL OR (m.from_product_id = sqlc.narg(from_product_id)::uuid AND m.from_price_key = sqlc.narg(from_price_key)::text))
  AND (sqlc.narg(after_at)::timestamptz IS NULL OR (m.created_at, m.id) < (sqlc.narg(after_at)::timestamptz, sqlc.narg(after_id)::uuid))
ORDER BY m.created_at DESC, m.id DESC
LIMIT sqlc.arg(row_limit)::int;
