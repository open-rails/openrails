-- name: EnsureDefaultCatalog :one
INSERT INTO billing.catalogs (merchant_id)
VALUES (sqlc.arg(merchant_id)::uuid)
ON CONFLICT (merchant_id) WHERE owner_subject IS NULL
DO UPDATE SET updated_at=billing.catalogs.updated_at
RETURNING *;

-- name: EnsureOwnedCatalog :one
INSERT INTO billing.catalogs (merchant_id,owner_subject)
VALUES (sqlc.arg(merchant_id)::uuid,sqlc.arg(owner_subject)::text)
ON CONFLICT (merchant_id,owner_subject) WHERE owner_subject IS NOT NULL
DO UPDATE SET updated_at=billing.catalogs.updated_at
RETURNING *;

-- name: GetCatalog :one
SELECT * FROM billing.catalogs
WHERE merchant_id=sqlc.arg(merchant_id)::uuid AND id=sqlc.arg(id)::uuid
  AND (sqlc.narg(catalog_id)::uuid IS NULL OR id=sqlc.narg(catalog_id)::uuid);

-- name: ListCatalogs :many
-- One keyset page, oldest first: rows after (after_at, after_id).
SELECT * FROM billing.catalogs
WHERE merchant_id=sqlc.arg(merchant_id)::uuid
  AND (sqlc.narg(catalog_id)::uuid IS NULL OR id=sqlc.narg(catalog_id)::uuid)
  AND (sqlc.narg(owner_subject)::text IS NULL OR owner_subject=sqlc.narg(owner_subject)::text)
  AND (sqlc.narg(after_at)::timestamptz IS NULL OR (created_at, id) > (sqlc.narg(after_at)::timestamptz, sqlc.narg(after_id)::uuid))
ORDER BY created_at, id
LIMIT sqlc.arg(fetch_limit)::int;

-- name: GetCatalogByOwner :one
SELECT * FROM billing.catalogs
WHERE merchant_id=sqlc.arg(merchant_id)::uuid AND owner_subject=sqlc.arg(owner_subject)::text
  AND (sqlc.narg(catalog_id)::uuid IS NULL OR id=sqlc.narg(catalog_id)::uuid);

-- name: LockCatalogKey :exec
SELECT pg_advisory_xact_lock(hashtextextended(sqlc.arg(lock_key)::text, 0));
