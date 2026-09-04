-- name: LockLiveMerchantForSecretWrite :one
SELECT id FROM openrails.merchants
WHERE id=sqlc.arg(id)::uuid AND deleted_at IS NULL
FOR UPDATE;

-- name: LockMerchantSecretCleanupRun :one
SELECT r.* FROM openrails.destructive_runs r
JOIN openrails.merchants m ON m.id=r.merchant_id
WHERE r.merchant_id=sqlc.arg(merchant_id)::uuid AND r.id=sqlc.arg(id)::uuid
  AND r.kind='merchant_purge' AND m.deleted_at IS NOT NULL
  AND r.coverage->>'database_purged'='true'
FOR UPDATE OF r,m;

-- name: MarkMerchantDatabasePurged :exec
UPDATE openrails.destructive_runs
SET coverage=coverage || '{"database_purged":true}'::jsonb,
    affected=sqlc.arg(affected)::jsonb
WHERE merchant_id=sqlc.arg(merchant_id)::uuid AND id=sqlc.arg(id)::uuid;

-- name: RecordMerchantSecretCleanup :execrows
UPDATE openrails.destructive_runs
SET status=sqlc.arg(status)::text,
    finished_at=CASE WHEN sqlc.arg(status)::text='completed' THEN now() ELSE NULL END,
    affected=COALESCE(affected,'{}'::jsonb) || jsonb_build_object('external_secrets_deleted',sqlc.arg(deleted)::int),
    coverage=coverage || jsonb_build_object('secret_cleanup_error',sqlc.narg(error)::text,'secret_cleanup_attempted_at',now())
WHERE merchant_id=sqlc.arg(merchant_id)::uuid AND id=sqlc.arg(id)::uuid;

-- CROSS-MERCHANT: committed tombstoned purge runs only. Per-run rows and cleanup
-- are handled under the captured merchant's scope.
-- name: ListPendingMerchantSecretCleanups :many
SELECT merchant_id,run_id FROM openrails.pending_merchant_secret_cleanups(
 sqlc.narg(after_run_id)::uuid,sqlc.arg(page_limit)::int);
