-- billing.payment_methods.

-- name: CreatePaymentMethod :execrows
INSERT INTO billing.payment_methods (
    id, tenant_subject_id, processor, vault_id, billing_id,
    initial_transaction_id, last_four, card_type, expiry_date,
    failure_reason, metadata, created_at, updated_at
) VALUES (
    $1, $2, $3, $4, sqlc.narg(billing_id),
    $5, sqlc.narg(last_four), sqlc.narg(card_type), sqlc.narg(expiry_date),
    sqlc.narg(failure_reason), sqlc.narg(metadata),
    COALESCE(NULLIF(sqlc.arg(created_at)::timestamptz, '0001-01-01 00:00:00+00'::timestamptz), now()),
    COALESCE(NULLIF(sqlc.arg(updated_at)::timestamptz, '0001-01-01 00:00:00+00'::timestamptz), now())
);

-- name: GetPaymentMethodByID :one
SELECT * FROM billing.payment_methods WHERE id = $1;

-- name: ListPaymentMethodsByIDs :many
SELECT * FROM billing.payment_methods WHERE id = ANY(sqlc.arg(ids)::uuid[]);

-- name: DeletePaymentMethod :execrows
DELETE FROM billing.payment_methods WHERE id = $1;

-- name: ListPaymentMethodsByTenantSubject :many
SELECT * FROM billing.payment_methods pm
WHERE pm.tenant_subject_id = $1
ORDER BY pm.created_at DESC;

-- name: CountPaymentMethodsByTenantSubject :one
SELECT count(*) FROM billing.payment_methods pm
WHERE pm.tenant_subject_id = $1;

-- name: ListPaymentMethodsByTenantSubjectPaged :many
SELECT * FROM billing.payment_methods pm
WHERE pm.tenant_subject_id = $1
ORDER BY pm.created_at DESC
LIMIT NULLIF(sqlc.arg(page_limit)::int, 0) OFFSET sqlc.arg(page_offset)::int;

-- name: GetPaymentMethodByVaultID :one
SELECT * FROM billing.payment_methods pm
WHERE pm.processor = $1 AND pm.vault_id = $2
LIMIT 1;

-- name: GetPaymentMethodByInitialTransactionID :one
SELECT * FROM billing.payment_methods pm
WHERE pm.processor = $1 AND pm.initial_transaction_id = $2
LIMIT 1;

-- name: UpdatePaymentMethod :execrows
UPDATE billing.payment_methods SET
    tenant_subject_id = $2,
    processor = $3,
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

-- name: ListPaymentMethodsByProcessors :many
SELECT * FROM billing.payment_methods pm
WHERE pm.processor = ANY(sqlc.arg(processors)::text[])
ORDER BY pm.created_at DESC;

-- name: ListPaymentMethodsByTenantSubjectProcessors :many
SELECT * FROM billing.payment_methods pm
WHERE pm.tenant_subject_id = $1
  AND pm.processor = ANY(sqlc.arg(processors)::text[])
ORDER BY pm.created_at DESC;

-- name: CountPaymentMethodForUser :one
SELECT count(*) FROM billing.payment_methods pm
WHERE pm.id = $1 AND pm.tenant_subject_id = $2;

-- name: ListPaymentMethodsByProcessor :many
SELECT * FROM billing.payment_methods pm
WHERE pm.processor = $1
ORDER BY pm.created_at DESC;
