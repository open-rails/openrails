-- openrails.product_access_grants (issue #250).

-- name: CreateProductAccessGrant :one
INSERT INTO openrails.product_access_grants (
    id, tenant_id, tenant_subject_id, product_id, source_type, source_id,
    payment_id, status, starts_at, ends_at, revoked_at, revoke_reason,
    created_at, updated_at
) VALUES (
    COALESCE(NULLIF(sqlc.arg(id)::uuid, '00000000-0000-0000-0000-000000000000'::uuid), uuidv7()),
    sqlc.arg(tenant_id)::uuid,
    $1, $2, $3,
    COALESCE(sqlc.arg(source_id)::text, ''),
    sqlc.narg(payment_id),
    COALESCE(NULLIF(sqlc.arg(status)::text, ''), 'active'),
    COALESCE(NULLIF(sqlc.arg(starts_at)::timestamptz, '0001-01-01 00:00:00+00'::timestamptz), now()),
    sqlc.narg(ends_at), sqlc.narg(revoked_at), sqlc.narg(revoke_reason),
    COALESCE(NULLIF(sqlc.arg(created_at)::timestamptz, '0001-01-01 00:00:00+00'::timestamptz), now()),
    COALESCE(NULLIF(sqlc.arg(updated_at)::timestamptz, '0001-01-01 00:00:00+00'::timestamptz), now())
)
RETURNING id;

-- name: GetProductAccessGrantBySource :one
SELECT * FROM openrails.product_access_grants pag
WHERE pag.tenant_id = $1
  AND pag.tenant_subject_id = $2
  AND pag.product_id = $3
  AND pag.source_id = $4
LIMIT 1;

-- name: HasActiveProductAccess :one
SELECT EXISTS (
    SELECT 1 FROM openrails.product_access_grants pag
    WHERE pag.tenant_id = $1
      AND pag.tenant_subject_id = $2
      AND pag.product_id = $3
      AND pag.status = 'active'
      AND pag.revoked_at IS NULL
      AND pag.starts_at <= sqlc.arg(at)::timestamptz
      AND (pag.ends_at IS NULL OR pag.ends_at > sqlc.arg(at)::timestamptz)
);

-- name: ListActiveProductAccessGrantsByTenantSubject :many
SELECT * FROM openrails.product_access_grants pag
WHERE pag.tenant_id = $1
  AND pag.tenant_subject_id = $2
  AND pag.status = 'active'
  AND pag.revoked_at IS NULL
  AND pag.starts_at <= sqlc.arg(at)::timestamptz
  AND (pag.ends_at IS NULL OR pag.ends_at > sqlc.arg(at)::timestamptz)
ORDER BY pag.created_at DESC;

-- name: ListProductAccessGrantsByTenantSubject :many
SELECT * FROM openrails.product_access_grants pag
WHERE pag.tenant_id = $1
  AND pag.tenant_subject_id = $2
ORDER BY pag.created_at DESC;

-- name: GetProductAccessGrantByID :one
SELECT * FROM openrails.product_access_grants pag
WHERE pag.tenant_id = $1 AND pag.id = $2;

-- name: RevokeProductAccessGrantByID :execrows
UPDATE openrails.product_access_grants pag SET
    status = 'revoked',
    revoked_at = sqlc.arg(now)::timestamptz,
    revoke_reason = sqlc.arg(reason),
    ends_at = sqlc.arg(now)::timestamptz,
    updated_at = sqlc.arg(now)::timestamptz
WHERE pag.tenant_id = $1
  AND pag.id = $2
  AND pag.status = 'active'
  AND pag.revoked_at IS NULL;

-- name: RevokeProductAccessGrantsByPayment :execrows
UPDATE openrails.product_access_grants pag SET
    status = 'revoked',
    revoked_at = sqlc.arg(now)::timestamptz,
    revoke_reason = sqlc.arg(reason),
    ends_at = sqlc.arg(now)::timestamptz,
    updated_at = sqlc.arg(now)::timestamptz
WHERE pag.tenant_id = $1
  AND pag.payment_id = $2
  AND pag.status = 'active'
  AND pag.revoked_at IS NULL;
