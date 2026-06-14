-- openrails.entitlement_grants.

-- name: CreateEntitlementGrant :one
-- RETURNING id matches bun's pk write-back for the uuidv7() default.
INSERT INTO openrails.entitlement_grants (
    id, merchant_id, merchant_subject_id, price_id, granted_by, reason, payment_id,
    duration_days, created_at
) VALUES (
    COALESCE(NULLIF(sqlc.arg(id)::uuid, '00000000-0000-0000-0000-000000000000'::uuid), uuidv7()),
    sqlc.arg(merchant_id)::uuid,
    $1, sqlc.narg(price_id), $2, $3, sqlc.narg(payment_id),
    sqlc.narg(duration_days),
    COALESCE(NULLIF(sqlc.arg(created_at)::timestamptz, '0001-01-01 00:00:00+00'::timestamptz), now())
)
RETURNING id;

-- name: GetEntitlementGrantByID :one
SELECT * FROM openrails.entitlement_grants WHERE id = $1;

-- name: CountEntitlementGrantsByMerchantSubject :one
SELECT count(*) FROM openrails.entitlement_grants ag WHERE ag.merchant_subject_id = $1;

-- name: ListEntitlementGrantsByMerchantSubject :many
SELECT * FROM openrails.entitlement_grants ag
WHERE ag.merchant_subject_id = $1
ORDER BY ag.created_at DESC
LIMIT NULLIF(sqlc.arg(page_limit)::int, 0) OFFSET sqlc.arg(page_offset)::int;

-- name: CountEntitlementGrantsByGrantedBy :one
SELECT count(*) FROM openrails.entitlement_grants ag WHERE ag.granted_by = $1;

-- name: ListEntitlementGrantsByGrantedBy :many
SELECT * FROM openrails.entitlement_grants ag
WHERE ag.granted_by = $1
ORDER BY ag.created_at DESC
LIMIT NULLIF(sqlc.arg(page_limit)::int, 0) OFFSET sqlc.arg(page_offset)::int;
