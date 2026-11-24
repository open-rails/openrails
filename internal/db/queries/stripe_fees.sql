-- Stripe fee reads (billing.stripe_fee_reads): the Stripe charges, refunds and
-- disputes whose processor fees the general ledger has yet to read.

-- ListDueStripeFeeReads: the reads due now, across merchants, by the due
-- index, each with its PSP's account.
-- name: ListDueStripeFeeReads :many
SELECT r.merchant_id, r.psp_id, r.ref, r.kind, r.due_at, r.attempts, p.account_id
FROM billing.stripe_fee_reads r
JOIN billing.psps p ON p.merchant_id = r.merchant_id AND p.id = r.psp_id
WHERE r.due_at <= now()
ORDER BY r.due_at, r.merchant_id, r.psp_id, r.ref
LIMIT sqlc.arg(row_limit)::int;

-- DoneStripeFeeRead: a read whose fees posted, unless its object was queued
-- again since the read took it.
-- name: DoneStripeFeeRead :execrows
DELETE FROM billing.stripe_fee_reads
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND psp_id = sqlc.arg(psp_id)::uuid AND ref = sqlc.arg(ref)::text
  AND due_at = sqlc.arg(due_at)::timestamptz;

-- DeferStripeFeeRead: a read that failed waits, unless its object was queued
-- again since the read took it; at park_after failures it waits for its
-- finding (infinity). Answers the failures so far.
-- name: DeferStripeFeeRead :one
UPDATE billing.stripe_fee_reads
SET due_at = CASE WHEN attempts + 1 >= sqlc.arg(park_after)::int THEN 'infinity'::timestamptz
                  ELSE now() + make_interval(secs => sqlc.arg(wait_seconds)::double precision) END,
    attempts = attempts + 1, last_error = sqlc.arg(error)::text
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND psp_id = sqlc.arg(psp_id)::uuid AND ref = sqlc.arg(ref)::text
  AND due_at = sqlc.arg(due_at)::timestamptz
RETURNING attempts;

-- GetStripeInvoicePaymentLinks: the charge or payment intent an invoice
-- payment was linked to (LinkStripeInvoicePayment).
-- name: GetStripeInvoicePaymentLinks :one
SELECT COALESCE(metadata->>'stripe_charge_id', '')::text AS charge_id,
       COALESCE(metadata->>'stripe_payment_intent_id', '')::text AS payment_intent_id
FROM billing.payments
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND psp_id = sqlc.arg(psp_id)::uuid AND rail = 'stripe'
  AND transaction_id = sqlc.arg(transaction_id)::text AND deleted_at IS NULL
ORDER BY created_at DESC
LIMIT 1;
