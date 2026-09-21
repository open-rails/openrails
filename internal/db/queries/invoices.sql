-- openrails.invoices: period invoices/statements. Arrears invoices become open
-- receivables at finalization; payments are allocated back to invoice_id.

-- name: ListInvoicePayers :many
-- Every (payer, currency) the period sweep must finalize: payers with #512
-- ledger money movement, and payers whose only activity is catalog-priced
-- usage that FinalizeInvoice still has to rate (no ledger row exists before
-- rating, so ledger_transfers alone never enumerates a usage-only payer such
-- as a metered platform fee). period_anchor is the first recorded activity,
-- from append-only created_at columns so anniversary windows never move.
SELECT customer_id::uuid AS customer_id, currency, MIN(period_anchor)::timestamptz AS period_anchor
FROM (
    SELECT customer_id, currency, MIN(created_at) AS period_anchor
    FROM openrails.ledger_transfers
    WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND customer_id IS NOT NULL
    GROUP BY customer_id, currency
    UNION ALL
    SELECT customer_id, currency, MIN(created_at) AS period_anchor
    FROM openrails.usage_events
    WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND pricing_authority = 'catalog'
    GROUP BY customer_id, currency
) activity
GROUP BY customer_id, currency
ORDER BY customer_id, currency;

-- name: GetInvoiceByPeriod :one
-- Idempotency key is per (payer, period, currency): one invoice per currency (#474).
SELECT * FROM openrails.invoices
WHERE merchant_id = $1 AND customer_id = $2
  AND period_from = $3 AND period_to = $4 AND currency = sqlc.arg(currency)
LIMIT 1;

-- name: InsertInvoice :exec
INSERT INTO openrails.invoices (
    id, merchant_id, customer_id, currency,
    invoice_number,
    period_from, period_to, usage_total, deposits_total, owed_accrued, owed_paid,
    closing_balance, subtotal_amount, total_amount, amount_paid, amount_due,
    line_items, money_movements, status, collection_method,
    issued_at, due_at, paid_at, voided_at, uncollectible_at,
    finalized_at, external_invoice_id,
    po_number, tax, billing_contacts, memo,
    created_at, updated_at
) VALUES (
    $1, $2, $3, $4,
    sqlc.narg(invoice_number),
    $5, $6, $7, $8, $9, $10,
    $11, sqlc.arg(subtotal_amount), sqlc.arg(total_amount), sqlc.arg(amount_paid), sqlc.arg(amount_due),
    COALESCE(sqlc.arg(line_items), '[]'::jsonb), COALESCE(sqlc.arg(money_movements), '{}'::jsonb),
    sqlc.arg(status), sqlc.arg(collection_method),
    sqlc.narg(issued_at), sqlc.narg(due_at), sqlc.narg(paid_at), sqlc.narg(voided_at),
    sqlc.narg(uncollectible_at), sqlc.narg(finalized_at),
    sqlc.narg(external_invoice_id),
    sqlc.narg(po_number), COALESCE(sqlc.arg(tax), '{}'::jsonb), COALESCE(sqlc.arg(billing_contacts), '[]'::jsonb), sqlc.narg(memo),
    sqlc.arg(created_at), sqlc.arg(updated_at)
);

-- name: InsertPendingInvoiceItem :exec
-- #726: invoice_items is the pending-accrual workspace only; rows are born
-- pending and only ever leave via AttachPendingInvoiceItemsToInvoice. The
-- statement itemization lives in invoices.line_items.
INSERT INTO openrails.invoice_items (
    id, merchant_id, customer_id, currency,
    source_type, source_id, invoice_at, amount, metadata,
    created_at, updated_at
) VALUES (
    $1, $2, $3, sqlc.arg(currency),
    sqlc.arg(source_type), sqlc.arg(source_id), sqlc.arg(invoice_at), sqlc.arg(amount),
    COALESCE(sqlc.arg(metadata), '{}'::jsonb),
    sqlc.arg(created_at), sqlc.arg(updated_at)
)
ON CONFLICT (merchant_id, customer_id, currency, source_type, source_id) DO NOTHING;

-- name: AttachPendingInvoiceItemsToInvoice :execrows
UPDATE openrails.invoice_items
SET invoice_id = sqlc.arg(invoice_id),
    status = 'invoiced',
    updated_at = sqlc.arg(now)::timestamptz
WHERE merchant_id = $1
  AND customer_id = $2
  AND currency = sqlc.arg(currency)
  AND invoice_id IS NULL
  AND status = 'pending'
  AND invoice_at >= sqlc.arg(period_from)::timestamptz
  AND invoice_at < sqlc.arg(period_to)::timestamptz;

-- name: SumPendingInvoiceItemAmountInPeriod :one
SELECT COALESCE(SUM(amount), 0)::bigint
FROM openrails.invoice_items
WHERE merchant_id = $1 AND customer_id = $2 AND currency = sqlc.arg(currency)
  AND invoice_id IS NULL AND status = 'pending'
  AND invoice_at >= sqlc.arg(period_from)::timestamptz
  AND invoice_at < sqlc.arg(period_to)::timestamptz;

-- name: ListInvoiceThresholdCandidates :many
--
-- or#897: the trigger amount is the BOUND billing policy's
-- collection_threshold_amount when the payer has one, else the merchant-wide
-- threshold, else the payer's own credit line. Resolved in SQL through the same
-- most-specific-wins rungs the admission path uses (money_settings.tier supplies
-- the tier rung), so a per-payer trigger costs no extra round trip.
SELECT s.customer_id, s.currency, MIN(ii.invoice_at)::timestamptz AS period_from, MIN(s.created_at)::timestamptz AS period_anchor
FROM openrails.money_settings s
JOIN openrails.invoice_items ii
  ON ii.merchant_id = s.merchant_id
 AND ii.customer_id = s.customer_id
 AND ii.currency = s.currency
 AND ii.invoice_id IS NULL
 AND ii.status = 'pending'
 AND ii.invoice_at < sqlc.arg(cutoff)::timestamptz
LEFT JOIN LATERAL (
    SELECT (p.policy ->> 'collection_threshold_amount')::bigint AS threshold
    FROM openrails.billing_policy_bindings b
    JOIN openrails.billing_policies p
      ON p.merchant_id = b.merchant_id AND p.name = b.policy_name
    WHERE b.merchant_id = s.merchant_id
      AND (b.customer_id = s.customer_id OR b.customer_id IS NULL)
      AND (b.tier = s.tier OR b.tier IS NULL)
    ORDER BY (b.customer_id IS NOT NULL) DESC, (b.tier IS NOT NULL) DESC
    LIMIT 1
) pol ON true
WHERE s.merchant_id = $1
  AND s.billing_mode = 'arrears'
  AND s.credit_limit_amount > 0
GROUP BY s.merchant_id, s.customer_id, s.currency, s.credit_limit_amount, pol.threshold
HAVING COALESCE(SUM(ii.amount), 0)::bigint + (
    SELECT COALESCE(SUM(i.amount_due), 0)::bigint
    FROM openrails.invoices i
    WHERE i.merchant_id = s.merchant_id
      AND i.customer_id = s.customer_id
      AND i.currency = s.currency
      AND i.status IN ('open', 'past_due')
      AND i.amount_due > 0
) >= COALESCE(
    pol.threshold,
    CASE WHEN sqlc.arg(min_threshold)::bigint > 0 THEN sqlc.arg(min_threshold)::bigint ELSE s.credit_limit_amount END)
ORDER BY period_from ASC;

-- name: ListChargeableOpenInvoices :many
SELECT i.id, i.merchant_id, i.customer_id, i.currency, i.amount_due,
       i.collection_failure_count, i.collection_failed_at,
       s.collection_payment_method_id::uuid AS collection_payment_method_id
FROM openrails.invoices i
JOIN openrails.money_settings s
  ON s.merchant_id = i.merchant_id
 AND s.customer_id = i.customer_id
 AND s.currency = i.currency
WHERE i.merchant_id = $1
  AND i.status IN ('open', 'past_due')
  AND i.amount_due > 0
  AND i.collection_method = 'charge_automatically'
  AND s.collection_payment_method_id IS NOT NULL
  AND i.collection_intent_id IS NULL
  AND (i.due_at IS NULL OR i.due_at <= sqlc.arg(now)::timestamptz)
  AND (
      (i.collection_failure_count = 0 AND i.next_collection_attempt_at IS NULL)
      OR i.next_collection_attempt_at <= sqlc.arg(now)::timestamptz
  )
  AND (sqlc.arg(min_threshold)::bigint <= 0 OR i.amount_due >= sqlc.arg(min_threshold)::bigint)
ORDER BY i.due_at NULLS FIRST, i.created_at ASC;

-- name: RecordInvoiceCollectionFailure :execrows
-- or#828/or#870, three buckets, three dispositions. The two arguments encode
-- them totally and disjointly, because collection.FailureAction does:
--
--   terminal                       -> bucket 3, or a bucket-1 schedule that ran
--                                     out. The invoice is uncollectible.
--   no terminal, NULL next attempt -> bucket 2. Charging STOPS because the
--                                     customer must fix their instrument.
--   a next attempt                 -> bucket 1. Still dunning.
--
-- Bucket 2 leaves `status` exactly where it was on purpose. A decline bucket
-- answers "what do we do about the CARD"; `past_due` is a reading of the CLOCK
-- (MarkInvoicesPastDue, due_at < now) and belongs to the delinquency axis
-- (or#878). Our decision to stop attempting must not age the customer's
-- invoice: it stays open, it stays collectible, and it stays theirs to settle.
UPDATE openrails.invoices
SET collection_failure_count = collection_failure_count + 1,
    collection_failed_at = COALESCE(collection_failed_at, sqlc.arg(now)::timestamptz),
    status = CASE
        WHEN sqlc.arg(terminal)::boolean THEN 'uncollectible'
        WHEN sqlc.narg(next_attempt_at)::timestamptz IS NULL THEN status
        ELSE 'past_due'
    END,
    next_collection_attempt_at = sqlc.narg(next_attempt_at)::timestamptz,
    last_collection_failure_code = sqlc.narg(failure_code),
    last_collection_failure_message = sqlc.narg(failure_message),
    uncollectible_at = CASE WHEN sqlc.arg(terminal)::boolean THEN sqlc.arg(now)::timestamptz ELSE NULL END,
    collection_intent_id = NULL,
    updated_at = sqlc.arg(now)::timestamptz
WHERE merchant_id = $1
  AND customer_id = $2
  AND id = sqlc.arg(invoice_id)
  AND status IN ('open', 'past_due')
  AND collection_intent_id = sqlc.arg(intent_id)::uuid;

-- name: ResumeStoppedInvoiceCollection :execrows
-- or#828 bucket-2 resume. A stopped invoice is one that failed at least once
-- and has NO next attempt scheduled — charging halted because the instrument
-- needs replacing. When the payer designates a collection payment method they
-- have done the thing the notice asked for, so those invoices become due again
-- immediately.
--
-- Untouched on purpose: `uncollectible` invoices (terminal needs an operator,
-- not a new card) and rows mid-claim or in-doubt, which the claim and verifier
-- machinery owns.
UPDATE openrails.invoices
SET next_collection_attempt_at = sqlc.arg(now)::timestamptz,
    updated_at = sqlc.arg(now)::timestamptz
WHERE merchant_id = $1
  AND customer_id = $2
  AND currency = sqlc.arg(currency)
  AND status IN ('open', 'past_due')
  AND amount_due > 0
  AND collection_method = 'charge_automatically'
  AND collection_failure_count > 0
  AND next_collection_attempt_at IS NULL
  AND collection_intent_id IS NULL;

-- name: MarkInvoicesPastDue :one
-- Invoice transitions and payer notifications commit in one statement. Collection
-- failures may already have set past_due; those invoices still need their notice.
WITH overdue AS (
    UPDATE openrails.invoices
    SET status = 'past_due', updated_at = sqlc.arg(now)::timestamptz
    WHERE merchant_id = sqlc.arg(merchant_id)::uuid
      AND status = 'open' AND amount_due > 0
      AND due_at IS NOT NULL AND due_at < sqlc.arg(now)::timestamptz
    RETURNING merchant_id, customer_id, id, invoice_number, amount_due, currency, due_at
), candidates AS (
    SELECT * FROM overdue
    UNION ALL
    SELECT merchant_id, customer_id, id, invoice_number, amount_due, currency, due_at
    FROM openrails.invoices
    WHERE merchant_id = sqlc.arg(merchant_id)::uuid
      AND status = 'past_due' AND amount_due > 0
      AND due_at IS NOT NULL AND due_at < sqlc.arg(now)::timestamptz
), notices AS (
    INSERT INTO openrails.notifications (id, merchant_id, customer_id, event_type, data, read_at, created_at)
    SELECT md5('invoice_overdue:' || id::text)::uuid, merchant_id, customer_id, 'invoice_overdue',
           jsonb_build_object('invoice_id', id,
                              'invoice_number', COALESCE(NULLIF(invoice_number, ''), id::text),
                              'amount_due', amount_due::text, 'currency', currency, 'due_at', due_at),
           NULL, sqlc.arg(now)::timestamptz
    FROM candidates
    ON CONFLICT (id) DO NOTHING
)
SELECT count(*) FROM overdue;

-- name: SumPendingInvoiceItemAmountBySourceInPeriod :many
-- #798: rated charge per accrual source for the statement's per-category
-- itemization at finalize. Metered accruals carry their rating identity
-- (e.g. metered:<meter>) in metadata->>'source' (the row source_id also
-- embeds the period window).
SELECT COALESCE(NULLIF(metadata ->> 'source', ''), source_id)::text AS source,
       COALESCE(SUM(amount), 0)::bigint AS amount,
       COUNT(*)::bigint AS item_count
FROM openrails.invoice_items
WHERE merchant_id = $1 AND customer_id = $2 AND currency = sqlc.arg(currency)
  AND invoice_id IS NULL AND status = 'pending'
  AND invoice_at >= sqlc.arg(period_from)::timestamptz
  AND invoice_at < sqlc.arg(period_to)::timestamptz
GROUP BY 1
ORDER BY 1 ASC;

-- name: ListPendingInvoiceItemsByPayer :many
-- #798: current accrued-but-uninvoiced charges (running-spend surface).
SELECT source_type,
       COALESCE(NULLIF(metadata ->> 'source', ''), source_id)::text AS source,
       amount, invoice_at
FROM openrails.invoice_items
WHERE merchant_id = $1 AND customer_id = $2 AND currency = sqlc.arg(currency)
  AND invoice_id IS NULL AND status = 'pending'
ORDER BY invoice_at ASC, source_id ASC;

-- name: ApplyInvoicePaymentSnapshot :execrows
UPDATE openrails.invoices
SET amount_paid = amount_paid + sqlc.arg(snapshot)::bigint,
    amount_due = GREATEST(0, amount_due - sqlc.arg(snapshot)::bigint),
    status = CASE WHEN amount_due - sqlc.arg(snapshot)::bigint <= 0 THEN 'paid' ELSE status END,
    paid_at = CASE WHEN amount_due - sqlc.arg(snapshot)::bigint <= 0 THEN sqlc.arg(now)::timestamptz ELSE paid_at END,
    next_collection_attempt_at = CASE WHEN amount_due - sqlc.arg(snapshot)::bigint <= 0 THEN NULL ELSE next_collection_attempt_at END,
    last_collection_failure_code = CASE WHEN amount_due - sqlc.arg(snapshot)::bigint <= 0 THEN NULL ELSE last_collection_failure_code END,
    last_collection_failure_message = CASE WHEN amount_due - sqlc.arg(snapshot)::bigint <= 0 THEN NULL ELSE last_collection_failure_message END,
    updated_at = sqlc.arg(now)::timestamptz
WHERE merchant_id = $1 AND customer_id = $2 AND id = sqlc.arg(invoice_id)
  AND status IN ('open', 'past_due')
  AND amount_due >= sqlc.arg(snapshot)::bigint;

-- name: SetInvoiceExternalID :execrows
UPDATE openrails.invoices
SET external_invoice_id = sqlc.arg(external_invoice_id),
    updated_at = sqlc.arg(now)::timestamptz
WHERE merchant_id = $1 AND customer_id = $2 AND id = sqlc.arg(invoice_id)
  AND (external_invoice_id IS NULL OR external_invoice_id = sqlc.arg(external_invoice_id));

-- name: InsertInvoicePayment :exec
INSERT INTO openrails.invoice_payments (
    id, merchant_id, customer_id, invoice_id, ledger_transfer_id,
    currency, amount, status, rail, rail_payment_id,
    failure_code, failure_reason, failure_message, attempted_at, settled_at, created_at, updated_at,
    payment_method_id, idempotency_key, psp_id
) VALUES (
    $1, $2, $3, sqlc.arg(invoice_id), sqlc.narg(ledger_transfer_id),
    sqlc.arg(currency), sqlc.arg(amount), sqlc.arg(status), sqlc.narg(rail),
    sqlc.narg(rail_payment_id), sqlc.narg(failure_code), sqlc.narg(failure_reason), sqlc.narg(failure_message),
    sqlc.arg(attempted_at), sqlc.narg(settled_at), sqlc.arg(created_at), sqlc.arg(updated_at),
    sqlc.narg(payment_method_id), sqlc.narg(idempotency_key), sqlc.narg(psp_id)::uuid
);

-- name: GetInvoicePaymentAttemptByKey :one
SELECT * FROM openrails.invoice_payments
WHERE merchant_id = $1
  AND customer_id = $2
  AND invoice_id = $3
  AND idempotency_key = sqlc.arg(idempotency_key)
LIMIT 1;

-- name: GetInvoicePaymentAttempt :one
SELECT * FROM openrails.invoice_payments
WHERE merchant_id = $1
  AND customer_id = $2
  AND invoice_id = $3
  AND id = sqlc.arg(attempt_id)
LIMIT 1;

-- name: FailClaimedInvoicePaymentAttempt :execrows
UPDATE openrails.invoice_payments
SET status = 'failed',
    rail = sqlc.narg(rail),
    rail_payment_id = sqlc.narg(rail_payment_id),
    failure_code = sqlc.narg(failure_code),
    failure_reason = sqlc.arg(failure_reason),
    failure_message = sqlc.narg(failure_message),
    updated_at = sqlc.arg(now)::timestamptz
WHERE merchant_id = $1
  AND customer_id = $2
  AND invoice_id = $3
  AND id = sqlc.arg(attempt_id)
  AND status = 'attempted';

-- name: SettleClaimedInvoicePaymentAttempt :execrows
UPDATE openrails.invoice_payments
SET status = 'settled',
    ledger_transfer_id = sqlc.arg(ledger_transfer_id),
    rail = sqlc.narg(rail),
    rail_payment_id = sqlc.narg(rail_payment_id),
    settled_at = sqlc.arg(now)::timestamptz,
    updated_at = sqlc.arg(now)::timestamptz
WHERE merchant_id = $1
  AND customer_id = $2
  AND invoice_id = $3
  AND id = sqlc.arg(attempt_id)
  AND status = 'attempted';

-- name: ListInvoicePaymentAttemptsByPayer :many
SELECT p.*
FROM openrails.invoice_payments p
JOIN openrails.invoices i
  ON i.merchant_id = p.merchant_id
 AND i.customer_id = p.customer_id
 AND i.id = p.invoice_id
WHERE p.merchant_id = $1
  AND p.customer_id = $2
  AND p.invoice_id = $3
ORDER BY p.created_at DESC, p.id DESC
LIMIT $4 OFFSET $5;

-- name: CountInvoicePaymentAttemptsByPayer :one
SELECT count(*)
FROM openrails.invoice_payments p
JOIN openrails.invoices i
  ON i.merchant_id = p.merchant_id
 AND i.customer_id = p.customer_id
 AND i.id = p.invoice_id
WHERE p.merchant_id = $1
  AND p.customer_id = $2
  AND p.invoice_id = $3;

-- name: ClaimInvoiceCollection :execrows
-- Points the invoice at its one live collection operation. Claiming says
-- nothing about lateness (status is the clock's reading, or#828/or#878);
-- reclaiming an `uncollectible` invoice reopens it (a manual retry undoing a
-- terminal outcome). The previous failure code stays as forensics.
UPDATE openrails.invoices
SET status = CASE WHEN status = 'uncollectible' THEN 'past_due' ELSE status END,
    next_collection_attempt_at = NULL,
    uncollectible_at = NULL,
    collection_intent_id = sqlc.arg(intent_id)::uuid,
    updated_at = sqlc.arg(now)::timestamptz
WHERE merchant_id = $1
  AND customer_id = $2
  AND id = sqlc.arg(invoice_id)
  AND status IN ('open', 'past_due', 'uncollectible')
  AND amount_due > 0
  AND collection_intent_id IS NULL;

-- name: ReleaseInvoiceCollection :execrows
-- The owning operation reached a terminal outcome that is not a decline
-- (settled, or provider-confirmed non-execution, which makes the invoice due
-- again at next_attempt_at).
UPDATE openrails.invoices
SET collection_intent_id = NULL,
    next_collection_attempt_at = sqlc.narg(next_attempt_at)::timestamptz,
    updated_at = sqlc.arg(now)::timestamptz
WHERE merchant_id = $1
  AND customer_id = $2
  AND id = sqlc.arg(invoice_id)
  AND collection_intent_id = sqlc.arg(intent_id)::uuid;

-- name: VoidInvoiceForPayer :one
UPDATE openrails.invoices
SET status = 'voided',
    amount_due = 0,
    voided_at = sqlc.arg(now)::timestamptz,
    updated_at = sqlc.arg(now)::timestamptz
WHERE merchant_id = $1 AND customer_id = $2 AND id = sqlc.arg(invoice_id)
  AND status IN ('draft', 'open', 'past_due')
  AND collection_intent_id IS NULL
RETURNING *;

-- name: MarkInvoiceUncollectibleForPayer :one
UPDATE openrails.invoices
SET status = 'uncollectible',
    uncollectible_at = sqlc.arg(now)::timestamptz,
    updated_at = sqlc.arg(now)::timestamptz
WHERE merchant_id = $1 AND customer_id = $2 AND id = sqlc.arg(invoice_id)
  AND status IN ('open', 'past_due')
  AND collection_intent_id IS NULL
RETURNING *;

-- name: ListInvoicesByPayer :many
SELECT * FROM openrails.invoices
WHERE merchant_id = $1 AND customer_id = $2
ORDER BY period_from DESC
LIMIT $3::int OFFSET $4::int;

-- name: CountInvoicesByPayer :one
SELECT count(*) FROM openrails.invoices
WHERE merchant_id = $1 AND customer_id = $2;

-- name: GetInvoiceForPayer :one
SELECT * FROM openrails.invoices
WHERE merchant_id = $1 AND customer_id = $2 AND id = $3
LIMIT 1;

-- name: GetInvoiceForPayerForUpdate :one
SELECT * FROM openrails.invoices
WHERE merchant_id = $1 AND customer_id = $2 AND id = $3
LIMIT 1
FOR UPDATE;

-- name: ListEncodedInvoiceAttemptsForArchive :many
-- The archive validates both copies of the generated payer-scoped coordinate
-- against the canonical collection operation before exporting or restoring it.
SELECT sqlc.embed(a), sqlc.embed(i),
    COALESCE(l.merchant_id = a.merchant_id AND l.customer_id = a.customer_id
        AND l.invoice_id = a.invoice_id AND l.currency = a.currency
        AND l.source = 'invoice_charge' AND l.source_id = i.idempotency_key
        AND l.operation = 'invoice_payment' AND l.transfer_type = 'owed_payment'
        AND l.amount = (i.payload->>'amount')::numeric, false)::boolean AS ledger_matches
FROM openrails.invoice_payments a
JOIN openrails.rail_intents i ON i.merchant_id = a.merchant_id
    AND i.idempotency_key = a.idempotency_key AND i.intent_type = 'invoice_collection'
LEFT JOIN openrails.ledger_transfers l ON l.merchant_id = a.merchant_id AND l.id = a.ledger_transfer_id
WHERE a.merchant_id = $1 AND a.idempotency_key LIKE 'invoice_collection:%';
