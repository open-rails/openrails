-- name: InsertChangeLogEntry :exec
-- Stamped by the database clock when the writer names no time.
INSERT INTO billing.change_log (merchant_id, occurred_at, actor, source, object_type, object_id, action, changes, reason, request_id, operation_id)
VALUES (sqlc.arg(merchant_id)::uuid, COALESCE(sqlc.narg(occurred_at)::timestamptz, clock_timestamp()), sqlc.arg(actor)::text, sqlc.arg(source)::text,
    sqlc.arg(object_type)::text, sqlc.arg(object_id)::text, sqlc.arg(action)::text, sqlc.arg(changes)::jsonb,
    sqlc.narg(reason)::text, sqlc.narg(request_id)::text, sqlc.narg(operation_id)::uuid);

-- name: ListChangeLog :many
-- Newest first, by object and time.
SELECT id, occurred_at, actor, source, object_type, object_id, action, changes, reason, request_id, operation_id
FROM billing.change_log
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND (sqlc.narg(object_type)::text IS NULL OR object_type = sqlc.narg(object_type)::text)
  AND (sqlc.narg(object_id)::text IS NULL OR object_id = sqlc.narg(object_id)::text)
  AND (sqlc.narg(since)::timestamptz IS NULL OR occurred_at >= sqlc.narg(since)::timestamptz)
  AND (sqlc.narg(until)::timestamptz IS NULL OR occurred_at < sqlc.narg(until)::timestamptz)
  AND (sqlc.narg(after_at)::timestamptz IS NULL OR (occurred_at, id) < (sqlc.narg(after_at)::timestamptz, sqlc.narg(after_id)::uuid))
ORDER BY occurred_at DESC, id DESC
LIMIT sqlc.arg(row_limit)::int;

-- name: ListConfigurationChangeLog :many
-- Every configuration document entry of a merchant, oldest first: what the
-- history last recorded of each document.
SELECT object_type, object_id, action, changes FROM billing.change_log
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND object_type IN ('merchant', 'messages', 'psp', 'custodian')
ORDER BY occurred_at, id;

-- name: LockConfigurationHistory :exec
-- One writer of a merchant's configuration history at a time: a write holds
-- it across its Vault write, so no load records the change first.
SELECT pg_advisory_xact_lock(hashtextextended('openrails.change_log:' || sqlc.arg(merchant_id)::uuid::text, 0));

-- name: ListPriceKeyMoves :many
-- A price key's moves, newest first: from occurred_at the key named the
-- price, or, archived, nothing. A deleted price's moves are left out.
SELECT c.id, c.occurred_at, p.id AS price_id, (c.changes->>'archived')::boolean AS archived
FROM billing.products product
JOIN billing.change_log c ON c.merchant_id = product.merchant_id AND c.object_type = 'price'
    AND c.object_id = product.key || ':' || sqlc.arg(key)::text AND c.action = 'move'
JOIN billing.prices p ON p.merchant_id = c.merchant_id AND p.product_id = product.id AND p.key = sqlc.arg(key)::text
    AND p.public_id = c.changes->>'price_id'
WHERE product.merchant_id = sqlc.arg(merchant_id)::uuid AND product.id = sqlc.arg(product_id)::uuid AND p.deleted_at IS NULL
  AND (sqlc.narg(after_at)::timestamptz IS NULL OR (c.occurred_at, c.id) < (sqlc.narg(after_at)::timestamptz, sqlc.narg(after_id)::uuid))
ORDER BY c.occurred_at DESC, c.id DESC
LIMIT sqlc.arg(fetch_limit)::int;
