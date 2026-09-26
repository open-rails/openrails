-- name: RegisterSolanaPayReference :one
INSERT INTO openrails.solana_pay_references (merchant_id, reference, checkout_session_id, kind, status, settle_until, watch_until, next_poll_at, created_at, updated_at)
VALUES (sqlc.arg(merchant_id)::uuid, sqlc.arg(reference)::text, sqlc.arg(checkout_session_id)::uuid, sqlc.arg(kind)::text, 'pending',
        sqlc.arg(settle_until)::timestamptz, sqlc.arg(watch_until)::timestamptz, sqlc.arg(now)::timestamptz, sqlc.arg(now)::timestamptz, sqlc.arg(now)::timestamptz)
ON CONFLICT (merchant_id, checkout_session_id) DO UPDATE SET updated_at = openrails.solana_pay_references.updated_at
RETURNING *;

-- name: GetSolanaPayReference :one
SELECT * FROM openrails.solana_pay_references
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND reference = sqlc.arg(reference)::text;

-- name: LockSolanaPayReference :one
SELECT * FROM openrails.solana_pay_references
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND reference = sqlc.arg(reference)::text
FOR UPDATE;

-- name: ClaimDueSolanaPayReferences :many
-- Cross-merchant poller claim. SKIP LOCKED plus the lease on next_poll_at
-- hands each due reference to one replica per pass.
UPDATE openrails.solana_pay_references r
SET next_poll_at = sqlc.arg(lease_until)::timestamptz
FROM (
    SELECT d.merchant_id, d.reference FROM openrails.solana_pay_references d
    WHERE d.next_poll_at <= sqlc.arg(now)::timestamptz
      AND (d.status = 'pending' OR (d.kind = 'purchase' AND d.watch_until > sqlc.arg(now)::timestamptz))
    ORDER BY d.next_poll_at
    LIMIT sqlc.arg(batch)::int
    FOR UPDATE SKIP LOCKED
) due
WHERE r.merchant_id = due.merchant_id AND r.reference = due.reference
RETURNING r.*;

-- name: ScheduleSolanaPayPoll :exec
UPDATE openrails.solana_pay_references SET next_poll_at = sqlc.arg(next_poll_at)::timestamptz
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND reference = sqlc.arg(reference)::text;

-- name: ConfirmSolanaPayReference :execrows
UPDATE openrails.solana_pay_references
SET status = 'confirmed', signature = sqlc.arg(signature)::text, updated_at = sqlc.arg(now)::timestamptz
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND reference = sqlc.arg(reference)::text AND status IN ('pending', 'expired');

-- name: ExpireSolanaPayReference :execrows
UPDATE openrails.solana_pay_references
SET status = 'expired', updated_at = sqlc.arg(now)::timestamptz
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND reference = sqlc.arg(reference)::text
  AND status = 'pending' AND settle_until < sqlc.arg(now)::timestamptz;

-- name: StoreSolanaPayBuiltTransaction :execrows
-- Compare-and-set on the previous build, so two concurrent wallet POSTs
-- cannot each hand out a different transaction.
UPDATE openrails.solana_pay_references
SET built_transaction = sqlc.arg(built_transaction)::text, built_valid_height = sqlc.arg(built_valid_height)::bigint, updated_at = sqlc.arg(now)::timestamptz
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND reference = sqlc.arg(reference)::text AND status = 'pending'
  AND built_valid_height IS NOT DISTINCT FROM sqlc.narg(previous_valid_height)::bigint;

-- name: GetSolanaPayReceipt :one
SELECT * FROM openrails.solana_pay_receipts
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND reference = sqlc.arg(reference)::text AND signature = sqlc.arg(signature)::text;

-- name: ListSolanaPayReceiptSignatures :many
SELECT signature FROM openrails.solana_pay_receipts
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND reference = sqlc.arg(reference)::text;

-- name: InsertSolanaPayReceipt :execrows
-- Conflicts on the reference's own row or on a signature already credited or
-- reviewed on any reference; either way nothing is written.
INSERT INTO openrails.solana_pay_receipts (merchant_id, reference, signature, checkout_session_id, disposition, review_reason, token_mint, expected_amount, received_amount, payer, landed_at, payment_id, created_at)
VALUES (sqlc.arg(merchant_id)::uuid, sqlc.arg(reference)::text, sqlc.arg(signature)::text, sqlc.arg(checkout_session_id)::uuid, sqlc.arg(disposition)::text,
        sqlc.narg(review_reason)::text, sqlc.arg(token_mint)::text, sqlc.arg(expected_amount)::bigint, sqlc.arg(received_amount)::bigint,
        sqlc.narg(payer)::text, sqlc.narg(landed_at)::timestamptz, sqlc.narg(payment_id)::uuid, sqlc.arg(now)::timestamptz)
ON CONFLICT DO NOTHING;

-- name: DeleteSettledSolanaPayReferences :execrows
-- GC: settled references past their watch window, in bounded batches. Their
-- ignored receipts go with them; credited and review receipts are kept.
WITH doomed AS (
    SELECT merchant_id, reference FROM openrails.solana_pay_references
    WHERE status <> 'pending' AND watch_until < sqlc.arg(now)::timestamptz
    ORDER BY watch_until
    LIMIT sqlc.arg(batch)::int
    FOR UPDATE SKIP LOCKED
), ignored AS (
    DELETE FROM openrails.solana_pay_receipts rc USING doomed d
    WHERE rc.merchant_id = d.merchant_id AND rc.reference = d.reference AND rc.disposition = 'ignored'
)
DELETE FROM openrails.solana_pay_references r USING doomed d
WHERE r.merchant_id = d.merchant_id AND r.reference = d.reference;
