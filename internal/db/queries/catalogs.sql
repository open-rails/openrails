-- name: EnsureDefaultCatalog :one
INSERT INTO openrails.catalogs (merchant_id)
VALUES (sqlc.arg(merchant_id)::uuid)
ON CONFLICT (merchant_id) WHERE owner_subject IS NULL
DO UPDATE SET updated_at=openrails.catalogs.updated_at
RETURNING *;

-- name: EnsureOwnedCatalog :one
INSERT INTO openrails.catalogs (merchant_id,owner_subject)
VALUES (sqlc.arg(merchant_id)::uuid,sqlc.arg(owner_subject)::text)
ON CONFLICT (merchant_id,owner_subject) WHERE owner_subject IS NOT NULL
DO UPDATE SET updated_at=openrails.catalogs.updated_at
RETURNING *;

-- name: GetCatalog :one
SELECT * FROM openrails.catalogs
WHERE merchant_id=sqlc.arg(merchant_id)::uuid AND id=sqlc.arg(id)::uuid
  AND (sqlc.narg(catalog_id)::uuid IS NULL OR id=sqlc.narg(catalog_id)::uuid);

-- name: ListCatalogs :many
SELECT * FROM openrails.catalogs
WHERE merchant_id=sqlc.arg(merchant_id)::uuid
  AND (sqlc.narg(catalog_id)::uuid IS NULL OR id=sqlc.narg(catalog_id)::uuid)
ORDER BY created_at,id
LIMIT sqlc.arg(page_limit)::int OFFSET sqlc.arg(page_offset)::int;

-- name: LockCatalogKey :exec
SELECT pg_advisory_xact_lock(hashtextextended(sqlc.arg(lock_key)::text, 0));
