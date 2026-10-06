-- name: GetCatalogRevision :one
SELECT catalog_revision FROM billing.merchants WHERE id=sqlc.arg(merchant_id)::uuid;

-- name: LockCatalogRevision :one
SELECT catalog_revision FROM billing.merchants WHERE id=sqlc.arg(merchant_id)::uuid FOR UPDATE;

-- name: AdvanceCatalogRevision :one
UPDATE billing.merchants SET catalog_revision=catalog_revision+1 WHERE id=sqlc.arg(merchant_id)::uuid RETURNING catalog_revision;

-- name: GetCatalogApplication :one
SELECT * FROM billing.catalog_applications WHERE merchant_id=sqlc.arg(merchant_id)::uuid AND application_id=sqlc.arg(application_id)::text;

-- name: InsertCatalogApplication :exec
INSERT INTO billing.catalog_applications (merchant_id,application_id,schema_version,request_sha256,base_revision,applied_revision,result)
VALUES (sqlc.arg(merchant_id)::uuid,sqlc.arg(application_id)::text,sqlc.arg(schema_version)::bigint,sqlc.arg(request_sha256)::bytea,sqlc.arg(base_revision)::bigint,sqlc.arg(applied_revision)::bigint,sqlc.arg(result)::jsonb);

-- name: SetCatalogBatchMerchant :exec
SELECT set_config('app.catalog_batch',sqlc.arg(merchant_id)::text,true);

-- name: LockCatalogApplicationPSP :one
SELECT * FROM billing.psps WHERE merchant_id=sqlc.arg(merchant_id)::uuid AND id=sqlc.arg(id)::uuid FOR SHARE NOWAIT;

-- name: LockCatalogKey :exec
SELECT pg_advisory_xact_lock(hashtextextended(sqlc.arg(lock_key)::text, 0));
