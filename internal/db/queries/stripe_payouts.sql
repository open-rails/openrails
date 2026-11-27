-- Stripe payout reads (billing.stripe_payout_reads): the payouts whose
-- deposit, return and reserve lines the general ledger has yet to read.

-- QueueStripePayoutRead: a notice of a payout reads it at once, its failures
-- forgotten.
-- name: QueueStripePayoutRead :exec
INSERT INTO billing.stripe_payout_reads AS r (merchant_id, psp_id, payout_id, due_at)
VALUES (sqlc.arg(merchant_id)::uuid, sqlc.arg(psp_id)::uuid, sqlc.arg(payout_id)::text, now())
ON CONFLICT (merchant_id, psp_id, payout_id) DO UPDATE SET due_at = now(), attempts = 0, last_error = NULL;

-- ListDueStripePayoutReads: the reads due now, across merchants, by the due
-- index, each with its PSP's account.
-- name: ListDueStripePayoutReads :many
SELECT r.merchant_id, r.psp_id, r.payout_id, r.due_at, r.attempts, p.account_id
FROM billing.stripe_payout_reads r
JOIN billing.psps p ON p.merchant_id = r.merchant_id AND p.id = r.psp_id
WHERE r.due_at <= now()
ORDER BY r.due_at, r.merchant_id, r.psp_id, r.payout_id
LIMIT sqlc.arg(row_limit)::int;

-- DoneStripePayoutRead: a payout read to its end, unless a notice queued it
-- again since the read took it.
-- name: DoneStripePayoutRead :execrows
DELETE FROM billing.stripe_payout_reads
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND psp_id = sqlc.arg(psp_id)::uuid AND payout_id = sqlc.arg(payout_id)::text
  AND due_at = sqlc.arg(due_at)::timestamptz;

-- WaitStripePayoutRead: a payout not yet at its end is read again at its
-- arrival date and each hour after, its failures forgotten, unless a notice
-- queued it again since.
-- name: WaitStripePayoutRead :execrows
UPDATE billing.stripe_payout_reads SET due_at = GREATEST(sqlc.arg(arrival)::timestamptz, now() + interval '1 hour'), attempts = 0, last_error = NULL
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND psp_id = sqlc.arg(psp_id)::uuid AND payout_id = sqlc.arg(payout_id)::text
  AND due_at = sqlc.arg(due_at)::timestamptz;

-- DeferStripePayoutRead: a read that failed waits, unless a notice queued it
-- again since; at park_after failures it waits for its finding (infinity).
-- Answers the failures so far.
-- name: DeferStripePayoutRead :one
UPDATE billing.stripe_payout_reads
SET due_at = CASE WHEN attempts + 1 >= sqlc.arg(park_after)::int THEN 'infinity'::timestamptz
                  ELSE now() + make_interval(secs => sqlc.arg(wait_seconds)::double precision) END,
    attempts = attempts + 1, last_error = sqlc.arg(error)::text
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND psp_id = sqlc.arg(psp_id)::uuid AND payout_id = sqlc.arg(payout_id)::text
  AND due_at = sqlc.arg(due_at)::timestamptz
RETURNING attempts;

-- UpsertBankAccount: the bank account a PSP names, with what it says of it
-- now.
-- name: UpsertBankAccount :one
INSERT INTO billing.bank_accounts AS b (merchant_id, psp_id, ref, name, last4, currency)
VALUES (sqlc.arg(merchant_id)::uuid, sqlc.arg(psp_id)::uuid, sqlc.arg(ref)::text, sqlc.narg(name)::text, sqlc.narg(last4)::text, sqlc.arg(currency)::text)
ON CONFLICT (merchant_id, psp_id, ref) DO UPDATE SET
    name = COALESCE(EXCLUDED.name, b.name), last4 = COALESCE(EXCLUDED.last4, b.last4),
    updated_at = CASE WHEN EXCLUDED.name IS DISTINCT FROM b.name OR EXCLUDED.last4 IS DISTINCT FROM b.last4 THEN now() ELSE b.updated_at END
RETURNING id;
