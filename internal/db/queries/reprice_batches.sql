-- openrails.reprice_batches (#773): header row for one bulk reprice operation.

-- name: CreateRepriceBatch :one
INSERT INTO openrails.reprice_batches (
    merchant_id, price_key, to_price_id, effective_at,
    subscriptions_matched, subscriptions_scheduled, subscriptions_skipped
) VALUES (
    sqlc.arg(merchant_id)::uuid, sqlc.narg(price_key)::text, sqlc.arg(to_price_id)::uuid, sqlc.arg(effective_at)::timestamptz,
    sqlc.arg(subscriptions_matched)::int, sqlc.arg(subscriptions_scheduled)::int, sqlc.arg(subscriptions_skipped)::int
)
RETURNING *;

-- #813: header row for one plan-migration operation (kind=plan_change).
-- name: CreatePlanMigrationBatch :one
INSERT INTO openrails.reprice_batches (
    merchant_id, to_price_id, effective_at, kind, source_price_id, fallback_policy,
    subscriptions_matched, subscriptions_scheduled, subscriptions_skipped, subscriptions_blocked
) VALUES (
    sqlc.arg(merchant_id)::uuid, sqlc.arg(to_price_id)::uuid, sqlc.arg(effective_at)::timestamptz,
    'plan_change', sqlc.arg(source_price_id)::uuid, sqlc.arg(fallback_policy)::text,
    sqlc.arg(subscriptions_matched)::int, sqlc.arg(subscriptions_scheduled)::int,
    sqlc.arg(subscriptions_skipped)::int, sqlc.arg(subscriptions_blocked)::int
)
RETURNING *;

-- name: GetRepriceBatchByID :one
SELECT * FROM openrails.reprice_batches WHERE id = $1;

-- name: ListRepriceBatches :many
SELECT * FROM openrails.reprice_batches
ORDER BY created_at DESC
LIMIT sqlc.arg(page_limit)::int OFFSET sqlc.arg(page_offset)::int;

-- #777: the console's price page needs "is there a pending migration for this
-- price key" without already knowing a batch id — list a key's bulk reprice
-- operations, most recent first.
-- name: ListRepriceBatchesByPriceKey :many
SELECT * FROM openrails.reprice_batches
WHERE price_key = sqlc.arg(price_key)::text
ORDER BY created_at DESC
LIMIT sqlc.arg(page_limit)::int OFFSET sqlc.arg(page_offset)::int;

-- #813: re-sync a plan-migration batch header after rail pushes degrade
-- scheduled rows to blocked — the header must always agree with its rows.
-- name: UpdatePlanMigrationBatchCounts :execrows
UPDATE openrails.reprice_batches SET
    subscriptions_scheduled = sqlc.arg(subscriptions_scheduled)::int,
    subscriptions_blocked = sqlc.arg(subscriptions_blocked)::int
WHERE id = sqlc.arg(id);
