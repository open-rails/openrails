-- name: InsertChangeLogEntry :exec
INSERT INTO billing.change_log (merchant_id, occurred_at, actor, source, object_type, object_id, action, changes, reason, request_id, operation_id)
VALUES (sqlc.arg(merchant_id)::uuid, sqlc.arg(occurred_at)::timestamptz, sqlc.arg(actor)::text, sqlc.arg(source)::text,
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
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND object_type IN ('merchant', 'psp', 'custodian')
ORDER BY occurred_at, id;

-- name: LockConfigurationHistory :exec
-- One writer of a merchant's configuration history at a time: a write holds
-- it across its Vault write, so no load records the change first.
SELECT pg_advisory_xact_lock(hashtextextended('openrails.change_log:' || sqlc.arg(merchant_id)::uuid::text, 0));
