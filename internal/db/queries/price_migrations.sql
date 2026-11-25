-- billing.price_migrations: one move of subscribers to another price.
-- Matched is counted at creation and skipped once every subscriber was
-- reached; the per-status counts come from the migration's scheduled changes
-- on read, and preparing from its admission, started until then.

-- name: CreatePriceMigration :one
INSERT INTO billing.price_migrations (
    merchant_id, from_price_id, from_product_id, from_price_key, to_price_id, effective_at,
    fallback_policy, subscriptions_matched, subscriptions_skipped, destructive_action_id
) VALUES (
    sqlc.arg(merchant_id)::uuid, sqlc.narg(from_price_id)::uuid, sqlc.narg(from_product_id)::uuid,
    sqlc.narg(from_price_key)::text, sqlc.arg(to_price_id)::uuid, sqlc.arg(effective_at)::timestamptz,
    sqlc.arg(fallback_policy)::text, sqlc.arg(subscriptions_matched)::int, 0, sqlc.narg(destructive_action_id)::uuid
)
RETURNING *;

-- name: SetPriceMigrationSkipped :execrows
-- Once a migration has reached its subscribers: those it made no move for,
-- skipped or gone from its prices before it reached them.
UPDATE billing.price_migrations m
SET subscriptions_skipped = GREATEST(0, m.subscriptions_matched - (
    SELECT count(*) FROM billing.scheduled_changes sc WHERE sc.merchant_id = m.merchant_id AND sc.price_migration_id = m.id))
WHERE m.merchant_id = sqlc.arg(merchant_id)::uuid AND m.id = sqlc.arg(id)::uuid;

-- name: LockPriceMigration :one
-- A page of a migration's walk holds its row: a cancel waits for the page,
-- and the next page sees the cancel.
SELECT * FROM billing.price_migrations
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid
FOR UPDATE;

-- name: CancelPriceMigration :execrows
UPDATE billing.price_migrations SET canceled_at = sqlc.arg(now)::timestamptz
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid AND canceled_at IS NULL;

-- name: GetPriceMigration :one
SELECT sqlc.embed(m), source.public_id AS from_price_public_id, target.public_id AS to_price_public_id, product.key AS product_key, c.scheduled, c.applied, c.canceled, c.blocked, c.skipped,
       coalesce(a.state = 'started', false)::boolean AS preparing
FROM billing.price_migrations m
LEFT JOIN billing.prices source ON source.merchant_id = m.merchant_id AND source.id = m.from_price_id
JOIN billing.prices target ON target.merchant_id = m.merchant_id AND target.id = m.to_price_id
LEFT JOIN billing.products product ON product.merchant_id = m.merchant_id AND product.id = m.from_product_id
LEFT JOIN billing.destructive_actions a ON a.merchant_id = m.merchant_id AND a.id = m.destructive_action_id
CROSS JOIN LATERAL (
    SELECT count(*) FILTER (WHERE sc.status = 'scheduled') AS scheduled,
           count(*) FILTER (WHERE sc.status = 'applied') AS applied,
           count(*) FILTER (WHERE sc.status = 'canceled') AS canceled,
           count(*) FILTER (WHERE sc.status = 'blocked') AS blocked,
           count(*) FILTER (WHERE sc.status = 'skipped') AS skipped
    FROM billing.scheduled_changes sc
    WHERE sc.merchant_id = m.merchant_id AND sc.price_migration_id = m.id
) c
WHERE m.merchant_id = sqlc.arg(merchant_id)::uuid AND m.id = sqlc.arg(id)::uuid;

-- name: ListPriceMigrationsByIDs :many
SELECT sqlc.embed(m), source.public_id AS from_price_public_id, target.public_id AS to_price_public_id, product.key AS product_key, c.scheduled, c.applied, c.canceled, c.blocked, c.skipped,
       coalesce(a.state = 'started', false)::boolean AS preparing
FROM billing.price_migrations m
LEFT JOIN billing.prices source ON source.merchant_id = m.merchant_id AND source.id = m.from_price_id
JOIN billing.prices target ON target.merchant_id = m.merchant_id AND target.id = m.to_price_id
LEFT JOIN billing.products product ON product.merchant_id = m.merchant_id AND product.id = m.from_product_id
LEFT JOIN billing.destructive_actions a ON a.merchant_id = m.merchant_id AND a.id = m.destructive_action_id
CROSS JOIN LATERAL (
    SELECT count(*) FILTER (WHERE sc.status = 'scheduled') AS scheduled,
           count(*) FILTER (WHERE sc.status = 'applied') AS applied,
           count(*) FILTER (WHERE sc.status = 'canceled') AS canceled,
           count(*) FILTER (WHERE sc.status = 'blocked') AS blocked,
           count(*) FILTER (WHERE sc.status = 'skipped') AS skipped
    FROM billing.scheduled_changes sc
    WHERE sc.merchant_id = m.merchant_id AND sc.price_migration_id = m.id
) c
WHERE m.merchant_id = sqlc.arg(merchant_id)::uuid AND m.id = ANY(sqlc.arg(ids)::uuid[])
ORDER BY m.created_at DESC, m.id DESC;

-- name: ListPriceMigrationsPage :many
SELECT sqlc.embed(m), source.public_id AS from_price_public_id, target.public_id AS to_price_public_id, product.key AS product_key, c.scheduled, c.applied, c.canceled, c.blocked, c.skipped,
       coalesce(a.state = 'started', false)::boolean AS preparing
FROM billing.price_migrations m
LEFT JOIN billing.prices source ON source.merchant_id = m.merchant_id AND source.id = m.from_price_id
JOIN billing.prices target ON target.merchant_id = m.merchant_id AND target.id = m.to_price_id
LEFT JOIN billing.products product ON product.merchant_id = m.merchant_id AND product.id = m.from_product_id
LEFT JOIN billing.destructive_actions a ON a.merchant_id = m.merchant_id AND a.id = m.destructive_action_id
CROSS JOIN LATERAL (
    SELECT count(*) FILTER (WHERE sc.status = 'scheduled') AS scheduled,
           count(*) FILTER (WHERE sc.status = 'applied') AS applied,
           count(*) FILTER (WHERE sc.status = 'canceled') AS canceled,
           count(*) FILTER (WHERE sc.status = 'blocked') AS blocked,
           count(*) FILTER (WHERE sc.status = 'skipped') AS skipped
    FROM billing.scheduled_changes sc
    WHERE sc.merchant_id = m.merchant_id AND sc.price_migration_id = m.id
) c
WHERE m.merchant_id = sqlc.arg(merchant_id)::uuid
  AND (sqlc.narg(from_product_id)::uuid IS NULL OR (m.from_product_id = sqlc.narg(from_product_id)::uuid AND m.from_price_key = sqlc.narg(from_price_key)::text))
  AND (sqlc.narg(after_at)::timestamptz IS NULL OR (m.created_at, m.id) < (sqlc.narg(after_at)::timestamptz, sqlc.narg(after_id)::uuid))
ORDER BY m.created_at DESC, m.id DESC
LIMIT sqlc.arg(row_limit)::int;

-- name: GetPriceMigrationByDestructiveAction :one
SELECT * FROM billing.price_migrations WHERE merchant_id=sqlc.arg(merchant_id)::uuid AND destructive_action_id=sqlc.arg(destructive_action_id)::uuid;
