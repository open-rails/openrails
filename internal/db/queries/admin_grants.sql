-- billing.admin_grants.

-- name: CreateAdminGrant :one
-- RETURNING id matches bun's pk write-back for the uuidv7() default.
INSERT INTO billing.admin_grants (
    id, tenant_subject_id, price_id, granted_by, reason, payment_id,
    duration_days, created_at
) VALUES (
    COALESCE(NULLIF(sqlc.arg(id)::uuid, '00000000-0000-0000-0000-000000000000'::uuid), uuidv7()),
    $1, sqlc.narg(price_id), $2, $3, sqlc.narg(payment_id),
    sqlc.narg(duration_days),
    COALESCE(NULLIF(sqlc.arg(created_at)::timestamptz, '0001-01-01 00:00:00+00'::timestamptz), now())
)
RETURNING id;

-- name: GetAdminGrantByID :one
SELECT * FROM billing.admin_grants WHERE id = $1;

-- name: CountAdminGrantsByTenantSubject :one
SELECT count(*) FROM billing.admin_grants ag WHERE ag.tenant_subject_id = $1;

-- name: ListAdminGrantsByTenantSubject :many
SELECT * FROM billing.admin_grants ag
WHERE ag.tenant_subject_id = $1
ORDER BY ag.created_at DESC
LIMIT NULLIF(sqlc.arg(page_limit)::int, 0) OFFSET sqlc.arg(page_offset)::int;

-- name: CountAdminGrantsByGrantedBy :one
SELECT count(*) FROM billing.admin_grants ag WHERE ag.granted_by = $1;

-- name: ListAdminGrantsByGrantedBy :many
SELECT * FROM billing.admin_grants ag
WHERE ag.granted_by = $1
ORDER BY ag.created_at DESC
LIMIT NULLIF(sqlc.arg(page_limit)::int, 0) OFFSET sqlc.arg(page_offset)::int;
