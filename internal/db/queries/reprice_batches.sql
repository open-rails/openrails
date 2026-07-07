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

-- name: GetRepriceBatchByID :one
SELECT * FROM openrails.reprice_batches WHERE id = $1;

-- name: ListRepriceBatches :many
SELECT * FROM openrails.reprice_batches
ORDER BY created_at DESC
LIMIT sqlc.arg(page_limit)::int OFFSET sqlc.arg(page_offset)::int;
