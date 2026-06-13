-- openrails.credit_types.

-- name: CreateCreditType :execrows
INSERT INTO openrails.credit_types (
    id, name, display_name, unit, decimal_places, is_active, created_at
) VALUES (
    $1, $2, $3, $4, $5, $6,
    COALESCE(NULLIF(sqlc.arg(created_at)::timestamptz, '0001-01-01 00:00:00+00'::timestamptz), now())
);

-- name: GetCreditTypeByID :one
SELECT * FROM openrails.credit_types WHERE id = $1;

-- name: GetCreditTypeByName :one
SELECT * FROM openrails.credit_types WHERE name = $1 LIMIT 1;

-- name: ListCreditTypes :many
SELECT * FROM openrails.credit_types ct
WHERE (NOT sqlc.arg(active_only)::boolean OR ct.is_active = true)
ORDER BY ct.created_at ASC;

-- name: UpdateCreditType :execrows
UPDATE openrails.credit_types SET
    name = $2,
    display_name = $3,
    unit = $4,
    decimal_places = $5,
    is_active = $6
WHERE id = $1;
