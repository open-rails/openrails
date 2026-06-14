-- openrails.checkout_sessions.

-- name: CreateCheckoutSession :execrows
INSERT INTO openrails.checkout_sessions (
    id, merchant_id, merchant_subject_id, price_id, mode, processor, status, amount,
    currency, expires_at, reference, transaction_id, payment_id,
    subscription_id, metadata, processor_fields, processor_state,
    idempotency_key, created_at, updated_at
) VALUES (
    $1, sqlc.arg(merchant_id)::uuid, $2, $3, $4, $5, $6, $7,
    COALESCE(NULLIF(sqlc.arg(currency)::text, ''), 'usd'),
    sqlc.narg(expires_at), sqlc.narg(reference), sqlc.narg(transaction_id),
    sqlc.narg(payment_id), sqlc.narg(subscription_id), sqlc.narg(metadata),
    sqlc.narg(processor_fields), sqlc.narg(processor_state),
    sqlc.narg(idempotency_key),
    COALESCE(NULLIF(sqlc.arg(created_at)::timestamptz, '0001-01-01 00:00:00+00'::timestamptz), now()),
    COALESCE(NULLIF(sqlc.arg(updated_at)::timestamptz, '0001-01-01 00:00:00+00'::timestamptz), now())
);

-- name: GetCheckoutSessionByID :one
SELECT * FROM openrails.checkout_sessions WHERE id = $1;

-- name: UpdateCheckoutSession :execrows
UPDATE openrails.checkout_sessions SET
    merchant_subject_id = $2,
    price_id = $3,
    mode = $4,
    processor = $5,
    status = $6,
    amount = $7,
    currency = $8,
    expires_at = sqlc.narg(expires_at),
    reference = sqlc.narg(reference),
    transaction_id = sqlc.narg(transaction_id),
    payment_id = sqlc.narg(payment_id),
    subscription_id = sqlc.narg(subscription_id),
    metadata = sqlc.narg(metadata),
    processor_fields = sqlc.narg(processor_fields),
    processor_state = sqlc.narg(processor_state),
    idempotency_key = sqlc.narg(idempotency_key),
    updated_at = sqlc.arg(updated_at)
WHERE id = $1;

-- name: BindSolanaCheckoutSession :execrows
UPDATE openrails.checkout_sessions SET
    reference = sqlc.arg(reference),
    processor_state = sqlc.arg(processor_state),
    updated_at = sqlc.arg(updated_at)
WHERE id = $1
  AND processor = 'solana'
  AND status = 'requires_action'
  AND (reference IS NULL OR reference = sqlc.arg(reference))
  AND (COALESCE(processor_state ->> 'payer', '') = '' OR processor_state ->> 'payer' = sqlc.arg(payer)::text);

-- name: GetCheckoutSessionByReference :one
SELECT * FROM openrails.checkout_sessions cs
WHERE cs.reference = $1
LIMIT 1;

-- name: GetLatestOpenCheckoutSession :one
SELECT * FROM openrails.checkout_sessions cs
WHERE cs.merchant_subject_id = $1
  AND cs.price_id = $2
  AND cs.processor = $3
  AND cs.status IN ('created', 'requires_action')
  AND (cs.expires_at IS NULL OR cs.expires_at > sqlc.arg(now)::timestamptz)
ORDER BY cs.created_at DESC
LIMIT 1;

-- name: ExpireCheckoutSessions :execrows
UPDATE openrails.checkout_sessions
SET status = 'expired', updated_at = sqlc.arg(now)
WHERE expires_at IS NOT NULL AND expires_at < sqlc.arg(now)::timestamptz
  AND status IN ('created', 'requires_action');
