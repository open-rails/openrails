-- billing.checkout_attempts.

-- name: GetCheckoutAttemptByID :one
SELECT * FROM billing.checkout_attempts WHERE checkout_attempts.merchant_id = sqlc.arg(merchant_id)::uuid AND id = $1
  AND deleted_at IS NULL;

-- name: UpdateCheckoutAttempt :execrows
UPDATE billing.checkout_attempts SET
    customer_id = sqlc.arg(customer_id)::uuid,
    rail = sqlc.arg(rail)::text,
    status = sqlc.arg(status)::text,
    amount = sqlc.narg(amount)::bigint,
    currency = sqlc.narg(currency)::text,
    expires_at = sqlc.narg(expires_at),
    reference = sqlc.narg(reference),
    transaction_id = sqlc.narg(transaction_id),
    payment_id = sqlc.narg(payment_id),
    subscription_id = sqlc.narg(subscription_id),
    metadata = sqlc.narg(metadata),
    rail_fields = sqlc.narg(rail_fields),
    rail_state = sqlc.narg(rail_state),
    psp_id = sqlc.arg(psp_id)::uuid,
    updated_at = sqlc.arg(updated_at)
WHERE checkout_attempts.merchant_id = sqlc.arg(merchant_id)::uuid AND id = $1
  AND deleted_at IS NULL
  AND order_id IS NOT NULL
  AND customer_id=sqlc.arg(customer_id)::uuid AND rail=sqlc.arg(rail)::text
  AND amount IS NOT DISTINCT FROM sqlc.narg(amount)::bigint AND currency IS NOT DISTINCT FROM sqlc.narg(currency)::text
  AND psp_id=sqlc.arg(psp_id)::uuid;

-- name: BindSolanaCheckoutAttempt :execrows
UPDATE billing.checkout_attempts SET
    reference = sqlc.arg(reference),
    rail_state = sqlc.arg(rail_state),
    updated_at = sqlc.arg(updated_at)
WHERE checkout_attempts.merchant_id = sqlc.arg(merchant_id)::uuid AND id = $1
  AND rail = 'solana'
  AND status = 'requires_action'
  AND (reference IS NULL OR reference = sqlc.arg(reference))
  AND (COALESCE(rail_state ->> 'payer', '') = '' OR rail_state ->> 'payer' = sqlc.arg(payer)::text)
  AND deleted_at IS NULL;

-- Binds a verified landed Solana transaction to the one checkout it settles.
-- Zero rows: the checkout is already settled by another transaction. A unique
-- violation (checkout_attempts_transaction_id_key): the transaction already
-- settles another checkout.
-- name: ClaimSolanaCheckoutSignature :execrows
UPDATE billing.checkout_attempts SET transaction_id = sqlc.arg(signature)::text
WHERE checkout_attempts.merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid
  AND rail = 'solana'
  AND deleted_at IS NULL
  AND (transaction_id IS NULL OR transaction_id = sqlc.arg(signature)::text);

-- name: GetCheckoutAttemptByReference :one
SELECT * FROM billing.checkout_attempts cs
WHERE cs.merchant_id = sqlc.arg(merchant_id)::uuid AND cs.reference = $1
  AND cs.deleted_at IS NULL
LIMIT 1;

-- name: ExpireCheckoutAttempts :execrows
UPDATE billing.checkout_attempts
SET status = 'expired', updated_at = sqlc.arg(now)
WHERE deleted_at IS NULL
  AND ctid IN (
    SELECT cs.ctid FROM billing.checkout_attempts cs
    WHERE cs.merchant_id = sqlc.arg(merchant_id)::uuid
      AND cs.expires_at IS NOT NULL AND cs.expires_at < sqlc.arg(now)::timestamptz
      AND cs.status IN ('created', 'requires_action')
      AND cs.deleted_at IS NULL
    LIMIT sqlc.arg(row_limit)::int
);

-- #511 LIFE plane (life.checkout_attempt.stale): expired-but-not-terminal
-- checkout attempts for a scope. Detection (read-only) for the Convergence Engine.
-- name: ListStaleCheckoutAttempts :many
SELECT id FROM billing.checkout_attempts
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND (sqlc.narg(customer_id)::uuid IS NULL OR customer_id = sqlc.narg(customer_id)::uuid)
  AND expires_at IS NOT NULL AND expires_at < sqlc.arg(now)::timestamptz
  AND status IN ('created', 'requires_action')
  AND deleted_at IS NULL
ORDER BY expires_at;

-- name: ExpireCheckoutAttemptByID :execrows
-- Repair for life.checkout_attempt.stale: mark one stale session expired.
UPDATE billing.checkout_attempts
SET status = 'expired', updated_at = sqlc.arg(now)::timestamptz
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid
  AND status IN ('created', 'requires_action')
  AND deleted_at IS NULL;

-- name: GetCheckoutCaptureAccountsForShare :one
-- Recheck the PSP and custodian identities after vendor metadata readback,
-- inside only the short local attachment transaction.
SELECT sqlc.embed(p),sqlc.embed(c) FROM billing.psps p
JOIN billing.custodians c ON c.merchant_id=p.merchant_id AND c.id=sqlc.arg(custodian_id)::uuid
WHERE p.merchant_id=sqlc.arg(merchant_id)::uuid AND p.id=sqlc.arg(psp_id)::uuid
FOR SHARE OF p,c;
