-- openrails.checkout_sessions.

-- name: CreateCheckoutSession :execrows
INSERT INTO openrails.checkout_sessions (
    id, merchant_id, customer_id, price_id, mode, rail, status, amount,
    currency, expires_at, reference, transaction_id, payment_id,
    subscription_id, metadata, rail_fields, rail_state,
    idempotency_key, rail_merchant_account_id, created_at, updated_at
) VALUES (
    $1, sqlc.arg(merchant_id)::uuid, $2, $3, $4, $5, $6, $7,
    sqlc.arg(currency),
    sqlc.narg(expires_at), sqlc.narg(reference), sqlc.narg(transaction_id),
    sqlc.narg(payment_id), sqlc.narg(subscription_id), sqlc.narg(metadata),
    sqlc.narg(rail_fields), sqlc.narg(rail_state),
    sqlc.narg(idempotency_key), sqlc.narg(rail_merchant_account_id),
    COALESCE(NULLIF(sqlc.arg(created_at)::timestamptz, '0001-01-01 00:00:00+00'::timestamptz), now()),
    COALESCE(NULLIF(sqlc.arg(updated_at)::timestamptz, '0001-01-01 00:00:00+00'::timestamptz), now())
);

-- name: GetCheckoutSessionByID :one
SELECT * FROM openrails.checkout_sessions WHERE id = $1;

-- name: UpdateCheckoutSession :execrows
UPDATE openrails.checkout_sessions SET
    customer_id = $2,
    price_id = $3,
    mode = $4,
    rail = $5,
    status = $6,
    amount = $7,
    currency = $8,
    expires_at = sqlc.narg(expires_at),
    reference = sqlc.narg(reference),
    transaction_id = sqlc.narg(transaction_id),
    payment_id = sqlc.narg(payment_id),
    subscription_id = sqlc.narg(subscription_id),
    metadata = sqlc.narg(metadata),
    rail_fields = sqlc.narg(rail_fields),
    rail_state = sqlc.narg(rail_state),
    idempotency_key = sqlc.narg(idempotency_key),
    rail_merchant_account_id = sqlc.narg(rail_merchant_account_id),
    updated_at = sqlc.arg(updated_at)
WHERE id = $1;

-- name: BindSolanaCheckoutSession :execrows
UPDATE openrails.checkout_sessions SET
    reference = sqlc.arg(reference),
    rail_state = sqlc.arg(rail_state),
    updated_at = sqlc.arg(updated_at)
WHERE id = $1
  AND rail = 'solana'
  AND status = 'requires_action'
  AND (reference IS NULL OR reference = sqlc.arg(reference))
  AND (COALESCE(rail_state ->> 'payer', '') = '' OR rail_state ->> 'payer' = sqlc.arg(payer)::text);

-- name: GetCheckoutSessionByReference :one
SELECT * FROM openrails.checkout_sessions cs
WHERE cs.reference = $1
LIMIT 1;

-- name: GetLatestOpenCheckoutSession :one
SELECT * FROM openrails.checkout_sessions cs
WHERE cs.customer_id = $1
  AND cs.price_id = $2
  AND cs.rail = $3
  AND cs.status IN ('created', 'requires_action')
  AND (cs.expires_at IS NULL OR cs.expires_at > sqlc.arg(now)::timestamptz)
ORDER BY cs.created_at DESC
LIMIT 1;

-- name: ExpireCheckoutSessions :execrows
UPDATE openrails.checkout_sessions
SET status = 'expired', updated_at = sqlc.arg(now)
WHERE expires_at IS NOT NULL AND expires_at < sqlc.arg(now)::timestamptz
  AND status IN ('created', 'requires_action');

-- #511 LIFE plane (life.checkout_session.stale): expired-but-not-terminal
-- checkout sessions for a scope. Detection (read-only) for the Convergence Engine.
-- name: ListStaleCheckoutSessions :many
SELECT id FROM openrails.checkout_sessions
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND (sqlc.narg(customer_id)::uuid IS NULL OR customer_id = sqlc.narg(customer_id)::uuid)
  AND expires_at IS NOT NULL AND expires_at < sqlc.arg(now)::timestamptz
  AND status IN ('created', 'requires_action')
ORDER BY expires_at;

-- name: ExpireCheckoutSessionByID :execrows
-- Repair for life.checkout_session.stale: mark one stale session expired.
UPDATE openrails.checkout_sessions
SET status = 'expired', updated_at = sqlc.arg(now)::timestamptz
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid
  AND status IN ('created', 'requires_action');
