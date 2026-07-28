-- Platform merchant directory (#721): cross-merchant operator reads over the
-- GLOBAL (non-RLS) openrails.merchants table, plus the directory-only
-- soft-delete/restore tombstone. Soft delete here is DIRECTORY state (list
-- exclusion + merchant-auth resolution failure); it is NOT the #225 gated purge
-- (internal/merchants/delete.go), which stays the only row-destroying path.

-- name: ListPlatformMerchants :many
SELECT id, slug, status, display_name, created_at, updated_at, deleted_at
FROM openrails.merchants
WHERE (sqlc.narg(status)::text IS NULL OR status = sqlc.narg(status)::text)
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg(page_limit)::bigint OFFSET sqlc.arg(page_offset)::bigint;

-- name: CountPlatformMerchants :one
SELECT count(*)
FROM openrails.merchants
WHERE (sqlc.narg(status)::text IS NULL OR status = sqlc.narg(status)::text);

-- name: GetPlatformMerchant :one
SELECT id, slug, status, display_name, created_at, updated_at, deleted_at
FROM openrails.merchants
WHERE id = $1;

-- name: SoftDeletePlatformMerchant :one
UPDATE openrails.merchants
   SET status     = 'deleted',
       deleted_at = COALESCE(deleted_at, current_timestamp),
       updated_at = current_timestamp
 WHERE id = $1
RETURNING id, slug, status, display_name, created_at, updated_at, deleted_at;

-- name: RestorePlatformMerchant :one
UPDATE openrails.merchants
   SET status     = 'active',
       deleted_at = NULL,
       updated_at = current_timestamp
 WHERE id = $1
RETURNING id, slug, status, display_name, created_at, updated_at, deleted_at;

-- Per-merchant list-view enrichment. Runs under a MerchantTx (RLS GUC pinned to
-- the merchant): psps + payments are merchant-isolated, so a
-- single cross-merchant JOIN is impossible under the openrails_app role — the
-- directory page loops cheap GUC-scoped index probes per row instead
-- (page-bounded).

-- name: ListPlatformMerchantRailsArmed :many
SELECT DISTINCT rail
FROM openrails.psps
WHERE merchant_id = $1 AND NOT archived
ORDER BY rail;

-- name: GetPlatformMerchantLastPayment :one
SELECT created_at
FROM openrails.payments
WHERE merchant_id = $1
  AND deleted_at IS NULL
ORDER BY created_at DESC
LIMIT 1;
