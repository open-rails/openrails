-- billing.reprice_batches (#773): header row for one bulk reprice operation.
-- Matched and skipped are facts of creation; the per-status counts are
-- derived from the batch's reprices on read.

-- name: CreateRepriceBatch :one
INSERT INTO billing.reprice_batches (
    merchant_id, price_key, to_price_id, effective_at, subscriptions_matched, subscriptions_skipped
) VALUES (
    sqlc.arg(merchant_id)::uuid, sqlc.narg(price_key)::text, sqlc.arg(to_price_id)::uuid, sqlc.arg(effective_at)::timestamptz,
    sqlc.arg(subscriptions_matched)::int, sqlc.arg(subscriptions_skipped)::int
)
RETURNING *;

-- #813: header row for one plan-migration operation (kind=plan_change).
-- name: CreatePlanMigrationBatch :one
INSERT INTO billing.reprice_batches (
    merchant_id, to_price_id, effective_at, kind, source_price_id, fallback_policy,
    subscriptions_matched, subscriptions_skipped
) VALUES (
    sqlc.arg(merchant_id)::uuid, sqlc.arg(to_price_id)::uuid, sqlc.arg(effective_at)::timestamptz,
    'plan_change', sqlc.arg(source_price_id)::uuid, NULLIF(sqlc.arg(fallback_policy)::text, ''),
    sqlc.arg(subscriptions_matched)::int, sqlc.arg(subscriptions_skipped)::int
)
RETURNING *;

-- name: GetRepriceBatch :one
SELECT sqlc.embed(b), c.scheduled, c.applied, c.canceled, c.blocked
FROM billing.reprice_batches b
CROSS JOIN LATERAL (
    SELECT count(*) FILTER (WHERE r.status = 'scheduled') AS scheduled,
           count(*) FILTER (WHERE r.status = 'applied') AS applied,
           count(*) FILTER (WHERE r.status = 'canceled') AS canceled,
           count(*) FILTER (WHERE r.status = 'blocked') AS blocked
    FROM billing.subscription_reprices r
    WHERE r.merchant_id = b.merchant_id AND r.reprice_batch_id = b.id
) c
WHERE b.merchant_id = sqlc.arg(merchant_id)::uuid AND b.id = sqlc.arg(id)::uuid;

-- name: ListRepriceBatchesByIDs :many
SELECT sqlc.embed(b), c.scheduled, c.applied, c.canceled, c.blocked
FROM billing.reprice_batches b
CROSS JOIN LATERAL (
    SELECT count(*) FILTER (WHERE r.status = 'scheduled') AS scheduled,
           count(*) FILTER (WHERE r.status = 'applied') AS applied,
           count(*) FILTER (WHERE r.status = 'canceled') AS canceled,
           count(*) FILTER (WHERE r.status = 'blocked') AS blocked
    FROM billing.subscription_reprices r
    WHERE r.merchant_id = b.merchant_id AND r.reprice_batch_id = b.id
) c
WHERE b.merchant_id = sqlc.arg(merchant_id)::uuid AND b.id = ANY(sqlc.arg(ids)::uuid[])
ORDER BY b.created_at DESC, b.id DESC;

-- name: ListRepriceBatchesPage :many
SELECT sqlc.embed(b), c.scheduled, c.applied, c.canceled, c.blocked
FROM billing.reprice_batches b
CROSS JOIN LATERAL (
    SELECT count(*) FILTER (WHERE r.status = 'scheduled') AS scheduled,
           count(*) FILTER (WHERE r.status = 'applied') AS applied,
           count(*) FILTER (WHERE r.status = 'canceled') AS canceled,
           count(*) FILTER (WHERE r.status = 'blocked') AS blocked
    FROM billing.subscription_reprices r
    WHERE r.merchant_id = b.merchant_id AND r.reprice_batch_id = b.id
) c
WHERE b.merchant_id = sqlc.arg(merchant_id)::uuid
  AND (sqlc.narg(price_key)::text IS NULL OR b.price_key = sqlc.narg(price_key)::text)
  AND (sqlc.arg(product_key)::text = '' OR EXISTS (
      SELECT 1 FROM billing.prices price
      JOIN billing.products product ON product.merchant_id=price.merchant_id AND product.id=price.product_id
      WHERE price.merchant_id=b.merchant_id AND price.id=b.to_price_id AND product.key=sqlc.arg(product_key)::text
  ))
  AND (sqlc.narg(after_at)::timestamptz IS NULL OR (b.created_at, b.id) < (sqlc.narg(after_at)::timestamptz, sqlc.narg(after_id)::uuid))
ORDER BY b.created_at DESC, b.id DESC
LIMIT sqlc.arg(row_limit)::int;
