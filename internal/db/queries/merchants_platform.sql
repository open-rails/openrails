-- Platform merchant directory (#721): cross-merchant operator reads over the
-- GLOBAL billing.merchants table, plus the directory-only
-- soft-delete/restore tombstone. Soft delete here is DIRECTORY state (list
-- exclusion + merchant-auth resolution failure); it is NOT the #225 gated purge
-- (internal/merchants/delete.go), which stays the only row-destroying path.

-- name: ListPlatformMerchants :many
-- One page of the directory, newest first, after a (created_at, id) cursor;
-- query searches current names only.
SELECT id, slug, status, created_at, updated_at, deleted_at
FROM billing.merchants
WHERE (sqlc.narg(status)::text IS NULL OR status = sqlc.narg(status)::text)
  AND (sqlc.narg(query)::text IS NULL OR strpos(slug, lower(sqlc.narg(query)::text)) > 0)
  AND (sqlc.narg(after_at)::timestamptz IS NULL OR (created_at, id) < (sqlc.narg(after_at)::timestamptz, sqlc.narg(after_id)::uuid))
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg(row_limit)::int;

-- name: GetPlatformMerchant :one
SELECT id, slug, status, created_at, updated_at, deleted_at
FROM billing.merchants
WHERE id = $1;

-- name: SoftDeletePlatformMerchant :one
UPDATE billing.merchants
   SET status     = 'deleted',
       deleted_at = COALESCE(deleted_at, current_timestamp),
       updated_at = current_timestamp
 WHERE id = $1
RETURNING id, slug, status, created_at, updated_at, deleted_at;

-- name: RestorePlatformMerchant :one
UPDATE billing.merchants
   SET status     = 'active',
       deleted_at = NULL,
       updated_at = current_timestamp
 WHERE id = $1
RETURNING id, slug, status, created_at, updated_at, deleted_at;

-- Per-merchant list-view enrichment. Runs under a MerchantTx per directory
-- row: psps + payments are merchant-owned, so the directory page loops cheap
-- per-merchant index probes instead of one cross-merchant JOIN
-- (page-bounded).

-- name: ListPlatformMerchantRailsArmed :many
-- The rails the merchant holds a current PSP identity on.
SELECT DISTINCT rail
FROM billing.psps
WHERE merchant_id = $1 AND superseded_at IS NULL
ORDER BY rail;

-- name: GetPlatformMerchantLastPayment :one
SELECT created_at
FROM billing.payments
WHERE merchant_id = $1
  AND deleted_at IS NULL
ORDER BY created_at DESC
LIMIT 1;

-- ListLedgerAuditMerchants is an on-demand fleet integrity sweep. It must list
-- the complete merchant directory so an audit cannot silently omit a tenant.
-- name: ListLedgerAuditMerchants :many
SELECT id, slug
FROM billing.merchants
ORDER BY slug;
