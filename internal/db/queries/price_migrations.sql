-- billing.price_migrations: one move of subscribers to another price.
-- Matched and skipped are facts of creation; the per-status counts come from
-- the migration's scheduled changes on read.

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
UPDATE billing.price_migrations SET subscriptions_skipped = sqlc.arg(subscriptions_skipped)::int
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid;

-- name: CancelPriceMigration :execrows
UPDATE billing.price_migrations SET canceled_at = sqlc.arg(now)::timestamptz
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid AND canceled_at IS NULL;

-- name: GetPriceMigration :one
SELECT sqlc.embed(m), source.public_id AS from_price_public_id, target.public_id AS to_price_public_id, product.key AS product_key, c.scheduled, c.applied, c.canceled, c.blocked
FROM billing.price_migrations m
LEFT JOIN billing.prices source ON source.merchant_id = m.merchant_id AND source.id = m.from_price_id
JOIN billing.prices target ON target.merchant_id = m.merchant_id AND target.id = m.to_price_id
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
SELECT sqlc.embed(m), source.public_id AS from_price_public_id, target.public_id AS to_price_public_id, product.key AS product_key, c.scheduled, c.applied, c.canceled, c.blocked
FROM billing.price_migrations m
LEFT JOIN billing.prices source ON source.merchant_id = m.merchant_id AND source.id = m.from_price_id
JOIN billing.prices target ON target.merchant_id = m.merchant_id AND target.id = m.to_price_id
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
SELECT sqlc.embed(m), source.public_id AS from_price_public_id, target.public_id AS to_price_public_id, product.key AS product_key, c.scheduled, c.applied, c.canceled, c.blocked
FROM billing.price_migrations m
LEFT JOIN billing.prices source ON source.merchant_id = m.merchant_id AND source.id = m.from_price_id
JOIN billing.prices target ON target.merchant_id = m.merchant_id AND target.id = m.to_price_id
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

-- name: GetPriceMigrationByDestructiveAction :one
SELECT * FROM billing.price_migrations WHERE merchant_id=sqlc.arg(merchant_id)::uuid AND destructive_action_id=sqlc.arg(destructive_action_id)::uuid;

-- Prepared means the complete cohort was scheduled by its originating command.
-- The finalizer runs after domain locks have been released.
-- name: ListSettledPriceMigrationActions :many
SELECT a.id FROM billing.destructive_actions a JOIN billing.price_migrations m
 ON m.merchant_id=a.merchant_id AND m.destructive_action_id=a.id
WHERE a.merchant_id=sqlc.arg(merchant_id)::uuid AND a.state IN ('started','uncertain')
 AND a.result='migration:'||m.id::text||':prepared'
 AND NOT EXISTS(SELECT 1 FROM billing.scheduled_changes sc WHERE sc.merchant_id=m.merchant_id AND sc.price_migration_id=m.id AND sc.status IN ('scheduled','blocked'))
 AND NOT EXISTS(SELECT 1 FROM billing.provider_intents p WHERE p.merchant_id=a.merchant_id AND p.destructive_action_id=a.id AND p.status NOT IN ('succeeded','failed_terminal','superseded','expired'))
ORDER BY a.id LIMIT sqlc.arg(row_limit)::int;

-- Directory addresses only; each finalization remains merchant scoped.
-- name: ListSettledPriceMigrationMerchants :many
SELECT DISTINCT a.merchant_id FROM billing.destructive_actions a JOIN billing.price_migrations m
 ON m.merchant_id=a.merchant_id AND m.destructive_action_id=a.id
WHERE a.state IN ('started','uncertain')
 AND a.result='migration:'||m.id::text||':prepared'
 AND NOT EXISTS(SELECT 1 FROM billing.scheduled_changes sc WHERE sc.merchant_id=m.merchant_id AND sc.price_migration_id=m.id AND sc.status IN ('scheduled','blocked'))
 AND NOT EXISTS(SELECT 1 FROM billing.provider_intents p WHERE p.merchant_id=a.merchant_id AND p.destructive_action_id=a.id AND p.status NOT IN ('succeeded','failed_terminal','superseded','expired'))
ORDER BY a.merchant_id LIMIT sqlc.arg(row_limit)::int;
