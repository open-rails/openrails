-- openrails.payment_methods.

-- name: CreatePaymentMethod :execrows
INSERT INTO openrails.payment_methods (
    id, merchant_id, customer_id, rail, vault_id, billing_id,
    initial_transaction_id, last_four, card_type, expiry_date,
    failure_reason, metadata, created_at, updated_at
) VALUES (
    $1, sqlc.arg(merchant_id)::uuid, $2, $3, $4, sqlc.narg(billing_id),
    $5, sqlc.narg(last_four), sqlc.narg(card_type), sqlc.narg(expiry_date),
    sqlc.narg(failure_reason), sqlc.narg(metadata),
    COALESCE(NULLIF(sqlc.arg(created_at)::timestamptz, '0001-01-01 00:00:00+00'::timestamptz), now()),
    COALESCE(NULLIF(sqlc.arg(updated_at)::timestamptz, '0001-01-01 00:00:00+00'::timestamptz), now())
);

-- name: GetPaymentMethodByID :one
SELECT * FROM openrails.payment_methods WHERE id = $1;

-- name: ListPaymentMethodsByIDs :many
SELECT * FROM openrails.payment_methods WHERE id = ANY(sqlc.arg(ids)::uuid[]);

-- name: DeletePaymentMethod :execrows
DELETE FROM openrails.payment_methods WHERE id = $1;

-- name: ListPaymentMethodsByCustomer :many
SELECT * FROM openrails.payment_methods pm
WHERE pm.customer_id = $1
ORDER BY pm.created_at DESC;

-- name: CountPaymentMethodsByCustomer :one
SELECT count(*) FROM openrails.payment_methods pm
WHERE pm.customer_id = $1;

-- name: ListPaymentMethodsByCustomerPaged :many
SELECT * FROM openrails.payment_methods pm
WHERE pm.customer_id = $1
ORDER BY pm.created_at DESC
LIMIT NULLIF(sqlc.arg(page_limit)::int, 0) OFFSET sqlc.arg(page_offset)::int;

-- name: GetPaymentMethodByVaultID :one
SELECT * FROM openrails.payment_methods pm
WHERE pm.rail = $1 AND pm.vault_id = $2
LIMIT 1;

-- name: GetPaymentMethodByInitialTransactionID :one
SELECT * FROM openrails.payment_methods pm
WHERE pm.rail = $1 AND pm.initial_transaction_id = $2
LIMIT 1;

-- name: UpdatePaymentMethod :execrows
UPDATE openrails.payment_methods SET
    customer_id = $2,
    rail = $3,
    vault_id = $4,
    billing_id = sqlc.narg(billing_id),
    initial_transaction_id = $5,
    last_four = sqlc.narg(last_four),
    card_type = sqlc.narg(card_type),
    expiry_date = sqlc.narg(expiry_date),
    failure_reason = sqlc.narg(failure_reason),
    metadata = sqlc.narg(metadata),
    updated_at = sqlc.arg(updated_at)
WHERE id = $1;

-- name: ListPaymentMethodsByRails :many
SELECT * FROM openrails.payment_methods pm
WHERE pm.rail = ANY(sqlc.arg(rails)::text[])
ORDER BY pm.created_at DESC;

-- name: ListPaymentMethodsByCustomerRails :many
SELECT * FROM openrails.payment_methods pm
WHERE pm.customer_id = $1
  AND pm.rail = ANY(sqlc.arg(rails)::text[])
ORDER BY pm.created_at DESC;

-- name: CountPaymentMethodForUser :one
SELECT count(*) FROM openrails.payment_methods pm
WHERE pm.id = $1 AND pm.customer_id = $2;

-- name: ListPaymentMethodsByRail :many
SELECT * FROM openrails.payment_methods pm
WHERE pm.rail = $1
ORDER BY pm.created_at DESC;
