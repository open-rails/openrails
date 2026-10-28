-- billing.scheduled_changes: a subscription's change waiting for its renewal.
-- At most one is scheduled per subscription (scheduled_changes_subscription_id_key).

-- name: CreateScheduledChange :one
INSERT INTO billing.scheduled_changes (
    merchant_id, subscription_id, from_price_id, price_id, quantity, effective_at,
    source, price_migration_id, status, acknowledged_short_notice, invoker, reason
) VALUES (
    sqlc.arg(merchant_id)::uuid, sqlc.arg(subscription_id)::uuid, sqlc.arg(from_price_id)::uuid,
    sqlc.arg(price_id)::uuid, sqlc.narg(quantity)::int, sqlc.arg(effective_at)::timestamptz,
    sqlc.arg(source)::text, sqlc.narg(price_migration_id)::uuid, 'scheduled', sqlc.arg(acknowledged_short_notice)::bool,
    sqlc.narg(invoker)::text, sqlc.narg(reason)::text
)
RETURNING *;

-- A migration's move its provider cannot take: terminal.
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

-- Each transition out of scheduled answers the change, so a migration's move
-- can settle its migration in the same transaction.

-- name: CancelScheduledChange :one
UPDATE billing.scheduled_changes SET status = 'canceled', canceled_at = sqlc.arg(now)::timestamptz
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid AND status = 'scheduled'
RETURNING *;

-- name: ApplyScheduledChange :one
UPDATE billing.scheduled_changes SET status = 'applied', applied_at = sqlc.arg(now)::timestamptz
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid AND status = 'scheduled'
RETURNING *;

-- A provider (Stripe) moved the subscription to price_id: the change that
-- asked for it is applied.
-- name: ApplyPendingScheduledChangeAtPrice :execrows
UPDATE billing.scheduled_changes SET status = 'applied', applied_at = sqlc.arg(now)::timestamptz
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND subscription_id = sqlc.arg(subscription_id)::uuid
  AND price_id = sqlc.arg(price_id)::uuid AND status = 'scheduled';

-- A migration's move its provider refused: terminal.
-- name: BlockScheduledChange :one
UPDATE billing.scheduled_changes SET status = 'blocked', blocked_reason = sqlc.arg(reason)::text
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid AND status = 'scheduled'
  AND source = 'migration'
RETURNING *;

-- A migration's move that no longer applies (the subscription ended or left
-- the source price, or the target is gone): terminal.
-- name: SkipScheduledChange :one
UPDATE billing.scheduled_changes SET status = 'skipped', blocked_reason = sqlc.arg(reason)::text
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid AND status = 'scheduled'
  AND source = 'migration'
RETURNING *;

-- name: ListScheduledMigrationChanges :many
SELECT * FROM billing.scheduled_changes
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND price_migration_id = sqlc.arg(price_migration_id)::uuid
  AND status = 'scheduled'
ORDER BY id;

-- A resolved move's push still waiting is no longer needed; one in flight
-- finds its move resolved.
-- name: SupersedeMovePushes :exec
UPDATE billing.provider_intents
SET status = 'superseded', last_failure_reason = 'the move resolved', lease_expires_at = NULL, updated_at = now()
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND subscription_id = sqlc.arg(subscription_id)::uuid
  AND intent_type = 'price_migration_push' AND payload->>'scheduled_change_id' = sqlc.arg(scheduled_change_id)::uuid::text
  AND status IN ('pending', 'failed_retryable');

-- A migration's command succeeds once Create recorded its every move and the
-- last of them resolved. A failed command, or one already finished, is left.
-- name: SettlePriceMigration :exec
UPDATE billing.destructive_actions a
SET state = 'succeeded', finished_at = sqlc.arg(now)::timestamptz,
    completion_sequence = coalesce(a.completion_sequence, nextval('billing.destructive_actions_admission_sequence_seq'::regclass))
FROM billing.price_migrations m
WHERE m.merchant_id = sqlc.arg(merchant_id)::uuid AND m.id = sqlc.arg(price_migration_id)::uuid
  AND a.merchant_id = m.merchant_id AND a.id = m.destructive_action_id
  AND a.state IN ('started', 'uncertain') AND a.result = 'migration:' || m.id::text || ':prepared'
  AND NOT EXISTS (SELECT 1 FROM billing.scheduled_changes s
                  WHERE s.merchant_id = m.merchant_id AND s.price_migration_id = m.id AND s.status = 'scheduled');
