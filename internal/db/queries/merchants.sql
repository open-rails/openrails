-- Merchant directory: provisioning, names and API hosts.

-- name: InsertMerchant :exec
INSERT INTO openrails.merchants (id, slug, status, permission_group_id)
VALUES (sqlc.arg(id)::uuid, sqlc.arg(slug)::text, 'active', sqlc.arg(group_id)::text);

-- Never adopts or rebinds an existing row (restore destinations).
-- name: InsertRestoredMerchant :one
INSERT INTO openrails.merchants (id, slug, status, permission_group_id)
VALUES (sqlc.arg(id)::uuid, sqlc.arg(slug)::text, 'active', NULLIF(sqlc.arg(group_id)::text, ''))
ON CONFLICT DO NOTHING
RETURNING id;

-- Includes retired rows so a group cannot silently acquire a new identity.
-- name: GetMerchantByGroupID :one
SELECT * FROM openrails.merchants WHERE permission_group_id = sqlc.arg(group_id)::text;

-- name: GetUnretiredLiveMerchant :one
SELECT * FROM openrails.merchants
WHERE id = sqlc.arg(id)::uuid AND deleted_at IS NULL AND retired_at IS NULL;

-- name: ListAllMerchantIDs :many
SELECT id FROM openrails.merchants ORDER BY id;

-- name: ListLiveMerchantsByGroupIDs :many
SELECT id, slug, COALESCE(display_name, '')::text AS display_name, permission_group_id::text AS group_id
FROM openrails.merchants
WHERE permission_group_id = ANY(sqlc.arg(group_ids)::text[]) AND deleted_at IS NULL
ORDER BY slug;

-- LIMIT 2: a second match means the caller must name the merchant.
-- name: ListLiveMerchantsByGroupID :many
SELECT * FROM openrails.merchants
WHERE permission_group_id = sqlc.arg(group_id)::text AND deleted_at IS NULL
LIMIT 2;

-- name: ListLiveMerchantsByAPIHost :many
SELECT * FROM openrails.merchants
WHERE api_host = sqlc.arg(api_host)::text AND deleted_at IS NULL
LIMIT 2;

-- name: SetMerchantDisplayName :execrows
UPDATE openrails.merchants SET display_name = sqlc.arg(display_name)::text, updated_at = current_timestamp
WHERE id = sqlc.arg(id)::uuid AND status = 'active' AND deleted_at IS NULL;

-- name: LockMerchantNameForRename :one
SELECT slug, slug_changed_at, now()::timestamptz AS now
FROM openrails.merchants
WHERE id = sqlc.arg(id)::uuid AND deleted_at IS NULL
FOR UPDATE;

-- name: RenameMerchant :exec
UPDATE openrails.merchants SET slug = sqlc.arg(slug)::text, slug_changed_at = now(), updated_at = now()
WHERE id = sqlc.arg(id)::uuid;

-- name: InsertMerchantSlugAlias :exec
INSERT INTO openrails.merchant_slug_aliases (slug, merchant_id, expires_at)
VALUES (
    sqlc.arg(slug)::text, sqlc.arg(merchant_id)::uuid,
    CASE WHEN sqlc.arg(forever)::boolean THEN NULL
         ELSE now() + sqlc.arg(retention_seconds)::float8 * interval '1 second' END
);

-- A live name or an unexpired former name; the slug returned is the current one.
-- name: GetMerchantBySlugOrAlias :one
SELECT m.id, m.slug, m.status, m.permission_group_id
FROM openrails.merchants m
WHERE m.slug = sqlc.arg(slug)::text AND m.deleted_at IS NULL
UNION ALL
SELECT m.id, m.slug, m.status, m.permission_group_id
FROM openrails.merchant_slug_aliases a
JOIN openrails.merchants m ON m.id = a.merchant_id
WHERE a.slug = sqlc.arg(slug)::text AND (a.expires_at IS NULL OR a.expires_at > now()) AND m.deleted_at IS NULL
LIMIT 1;

-- name: MerchantAPIHostTaken :one
SELECT EXISTS (
    SELECT 1 FROM openrails.merchants
    WHERE api_host = sqlc.arg(api_host)::text AND id <> sqlc.arg(id)::uuid AND deleted_at IS NULL
);

-- name: SetMerchantAPIHost :execrows
UPDATE openrails.merchants SET api_host = NULLIF(sqlc.arg(api_host)::text, ''), updated_at = current_timestamp
WHERE id = sqlc.arg(id)::uuid AND deleted_at IS NULL;

-- name: UpsertMerchantAPIHostClaim :one
INSERT INTO openrails.merchant_api_host_claims (merchant_id, api_host, token)
SELECT id, sqlc.arg(api_host)::text, sqlc.arg(token)::text
FROM openrails.merchants WHERE id = sqlc.arg(merchant_id)::uuid AND deleted_at IS NULL
ON CONFLICT (merchant_id) DO UPDATE
    SET api_host = EXCLUDED.api_host, token = EXCLUDED.token, created_at = now()
RETURNING *;

-- name: GetMerchantAPIHostClaim :one
SELECT * FROM openrails.merchant_api_host_claims WHERE merchant_id = sqlc.arg(merchant_id)::uuid;

-- name: DeleteProvenMerchantAPIHostClaim :execrows
DELETE FROM openrails.merchant_api_host_claims
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND api_host = sqlc.arg(api_host)::text AND token = sqlc.arg(token)::text;

-- name: DeleteMerchantAPIHostClaim :exec
DELETE FROM openrails.merchant_api_host_claims WHERE merchant_id = sqlc.arg(merchant_id)::uuid;

-- name: RecordPurgeInventory :one
INSERT INTO openrails.maintenance_runs (merchant_id, kind, status, inventory_manifest, inventory_total_rows, finished_at)
VALUES (sqlc.arg(merchant_id)::uuid, 'purge_inventory', 'completed', sqlc.arg(manifest)::jsonb,
        (sqlc.arg(manifest)::jsonb ->> 'total_rows')::bigint, current_timestamp)
RETURNING id;

-- name: CountMatchingPurgeInventories :one
SELECT count(*) FROM openrails.maintenance_runs
WHERE id = sqlc.arg(id)::uuid AND merchant_id = sqlc.arg(merchant_id)::uuid
  AND kind = 'purge_inventory' AND status = 'completed'
  AND inventory_total_rows = sqlc.arg(total_rows)::bigint
  AND inventory_manifest -> 'row_counts' = sqlc.arg(row_counts)::jsonb;

-- name: LockMerchantManifestBootstrap :exec
SELECT pg_advisory_lock(sqlc.arg(lock_key)::bigint);
