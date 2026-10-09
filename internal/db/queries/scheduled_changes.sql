-- billing.scheduled_changes: a subscription's change waiting for its renewal.
-- At most one is scheduled per subscription (scheduled_changes_subscription_id_key).

-- name: CreateScheduledChange :one
INSERT INTO billing.scheduled_changes (
    merchant_id, subscription_id, from_price_id, price_id, quantity, effective_at,
    source, price_migration_id, status, acknowledged_short_notice
) VALUES (
    sqlc.arg(merchant_id)::uuid, sqlc.arg(subscription_id)::uuid, sqlc.arg(from_price_id)::uuid,
    sqlc.arg(price_id)::uuid, sqlc.narg(quantity)::int, sqlc.arg(effective_at)::timestamptz,
    sqlc.arg(source)::text, sqlc.narg(price_migration_id)::uuid, 'scheduled', sqlc.arg(acknowledged_short_notice)::bool
)
RETURNING *;

-- A migration's move its provider cannot take. Terminal unless the reason is
-- a push failure, which the re-driver retries.
-- name: CreateBlockedScheduledChange :one
INSERT INTO billing.scheduled_changes (
    merchant_id, subscription_id, from_price_id, price_id, effective_at,
    source, price_migration_id, status, blocked_reason, acknowledged_short_notice
) VALUES (
    sqlc.arg(merchant_id)::uuid, sqlc.arg(subscription_id)::uuid, sqlc.arg(from_price_id)::uuid,
    sqlc.arg(price_id)::uuid, sqlc.arg(effective_at)::timestamptz,
    'migration', sqlc.arg(price_migration_id)::uuid, 'blocked', sqlc.arg(blocked_reason)::text, false
)
RETURNING *;

-- name: GetScheduledChange :one
SELECT * FROM billing.scheduled_changes
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid;

-- name: GetPendingScheduledChange :one
SELECT * FROM billing.scheduled_changes
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND subscription_id = sqlc.arg(subscription_id)::uuid
  AND status = 'scheduled';

-- The pending changes of a page of subscriptions.
-- name: ListPendingScheduledChanges :many
SELECT * FROM billing.scheduled_changes
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND subscription_id = ANY(sqlc.arg(subscription_ids)::uuid[])
  AND status = 'scheduled';

-- name: CancelScheduledChange :execrows
UPDATE billing.scheduled_changes SET status = 'canceled', canceled_at = sqlc.arg(now)::timestamptz
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid AND status = 'scheduled';

-- name: ApplyScheduledChange :execrows
UPDATE billing.scheduled_changes SET status = 'applied', applied_at = sqlc.arg(now)::timestamptz
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid AND status = 'scheduled';

-- A provider (Stripe) moved the subscription to price_id: the change that
-- asked for it is applied.
-- name: ApplyPendingScheduledChangeAtPrice :execrows
UPDATE billing.scheduled_changes SET status = 'applied', applied_at = sqlc.arg(now)::timestamptz
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND subscription_id = sqlc.arg(subscription_id)::uuid
  AND price_id = sqlc.arg(price_id)::uuid AND status = 'scheduled';

-- name: BlockScheduledChange :execrows
UPDATE billing.scheduled_changes SET status = 'blocked', blocked_reason = sqlc.arg(blocked_reason)::text
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid AND status = 'scheduled'
  AND source = 'migration';

-- The re-driver takes a push-failed move back; the unique index refuses it
-- when the subscription scheduled another change meanwhile.
-- name: UnblockScheduledChange :execrows
UPDATE billing.scheduled_changes SET status = 'scheduled', blocked_reason = NULL
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid AND status = 'blocked';

-- name: ListScheduledMigrationChanges :many
SELECT * FROM billing.scheduled_changes
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND price_migration_id = sqlc.arg(price_migration_id)::uuid
  AND status = 'scheduled'
ORDER BY id;

-- Push-failed moves of migrations still running.
-- name: ListRedrivableScheduledChanges :many
SELECT c.* FROM billing.scheduled_changes c
JOIN billing.price_migrations m ON m.merchant_id = c.merchant_id AND m.id = c.price_migration_id
WHERE c.merchant_id = sqlc.arg(merchant_id)::uuid
  AND c.status = 'blocked' AND c.blocked_reason LIKE 'rail_push_failed:%'
  AND m.canceled_at IS NULL
ORDER BY c.created_at
LIMIT sqlc.arg(batch_size)::int;

-- CROSS-MERCHANT: merchants holding a push-failed move. Ids only; the
-- re-driver reads the rows per merchant.
-- name: ListRedrivableScheduledChangeMerchants :many
SELECT DISTINCT c.merchant_id
FROM billing.scheduled_changes c
WHERE c.status = 'blocked' AND c.blocked_reason LIKE 'rail_push_failed:%'
LIMIT sqlc.arg(merchant_limit)::int;
