-- openrails.entitlement_features + openrails.product_entitlement_features (#245).

-- name: CreateEntitlementFeature :one
INSERT INTO openrails.entitlement_features (
    id, tenant_id, lookup_key, name, active, metadata, created_at, updated_at
) VALUES (
    COALESCE(NULLIF(sqlc.arg(id)::uuid, '00000000-0000-0000-0000-000000000000'::uuid), uuidv7()),
    sqlc.arg(tenant_id)::uuid,
    $1, $2, $3, sqlc.narg(metadata),
    COALESCE(NULLIF(sqlc.arg(created_at)::timestamptz, '0001-01-01 00:00:00+00'::timestamptz), now()),
    COALESCE(NULLIF(sqlc.arg(updated_at)::timestamptz, '0001-01-01 00:00:00+00'::timestamptz), now())
)
RETURNING id;

-- name: UpdateEntitlementFeature :execrows
UPDATE openrails.entitlement_features ef SET
    name = $2,
    active = $3,
    metadata = sqlc.narg(metadata),
    updated_at = sqlc.arg(updated_at)
WHERE ef.id = $1 AND ef.tenant_id = sqlc.arg(tenant_id);

-- name: ListEntitlementFeatures :many
SELECT * FROM openrails.entitlement_features ef
WHERE ef.tenant_id = $1
ORDER BY ef.created_at DESC;

-- name: GetEntitlementFeatureByID :one
SELECT * FROM openrails.entitlement_features ef
WHERE ef.id = $1 AND ef.tenant_id = $2;

-- name: GetEntitlementFeatureByLookupKey :one
SELECT * FROM openrails.entitlement_features ef
WHERE ef.lookup_key = $1 AND ef.tenant_id = $2;

-- name: ListEntitlementFeaturesByLookupKeys :many
SELECT * FROM openrails.entitlement_features ef
WHERE ef.tenant_id = $1
  AND ef.lookup_key = ANY(sqlc.arg(lookup_keys)::text[]);

-- name: CreateProductEntitlementFeature :one
INSERT INTO openrails.product_entitlement_features (
    id, tenant_id, product_id, entitlement_feature_id, duration_days,
    metadata, created_at, updated_at
) VALUES (
    COALESCE(NULLIF(sqlc.arg(id)::uuid, '00000000-0000-0000-0000-000000000000'::uuid), uuidv7()),
    sqlc.arg(tenant_id)::uuid,
    $1, $2, sqlc.narg(duration_days), sqlc.narg(metadata),
    COALESCE(NULLIF(sqlc.arg(created_at)::timestamptz, '0001-01-01 00:00:00+00'::timestamptz), now()),
    COALESCE(NULLIF(sqlc.arg(updated_at)::timestamptz, '0001-01-01 00:00:00+00'::timestamptz), now())
)
RETURNING id;

-- name: DeleteProductEntitlementFeature :execrows
DELETE FROM openrails.product_entitlement_features pef
WHERE pef.id = $1 AND pef.tenant_id = $2;

-- name: ListProductEntitlementFeatures :many
SELECT sqlc.embed(pef), sqlc.embed(ef)
FROM openrails.product_entitlement_features pef
JOIN openrails.entitlement_features ef ON ef.id = pef.entitlement_feature_id
WHERE pef.product_id = $1 AND pef.tenant_id = $2
ORDER BY pef.created_at ASC;

-- name: GetProductEntitlementFeatureByID :one
SELECT * FROM openrails.product_entitlement_features pef
WHERE pef.id = $1 AND pef.tenant_id = $2;
