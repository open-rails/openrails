-- openrails.invoices: period invoices/statements. Arrears invoices become open
-- receivables at finalization; payments are allocated back to invoice_id.

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

-- name: SumPendingInvoiceItemAmount :one
SELECT COALESCE(SUM(amount), 0)::bigint
FROM openrails.invoice_items
WHERE merchant_id = $1 AND customer_id = $2 AND currency = sqlc.arg(currency)
  AND invoice_id IS NULL AND status = 'pending';

-- name: SumPendingInvoiceItemAmountInPeriod :one
SELECT COALESCE(SUM(amount), 0)::bigint
FROM openrails.invoice_items
WHERE merchant_id = $1 AND customer_id = $2 AND currency = sqlc.arg(currency)
  AND invoice_id IS NULL AND status = 'pending'
  AND invoice_at >= sqlc.arg(period_from)::timestamptz
  AND invoice_at < sqlc.arg(period_to)::timestamptz;

-- name: SumOpenInvoiceAmountDue :one
SELECT COALESCE(SUM(amount_due), 0)::bigint
FROM openrails.invoices
WHERE merchant_id = $1 AND customer_id = $2 AND currency = sqlc.arg(currency)
  AND status IN ('open', 'past_due') AND amount_due > 0;

-- name: ListInvoiceThresholdCandidates :many
SELECT s.customer_id, s.currency, MIN(ii.invoice_at)::timestamptz AS period_from, MIN(s.created_at)::timestamptz AS period_anchor
FROM openrails.money_settings s
JOIN openrails.invoice_items ii
  ON ii.merchant_id = s.merchant_id
 AND ii.customer_id = s.customer_id
 AND ii.currency = s.currency
 AND ii.invoice_id IS NULL
 AND ii.status = 'pending'
 AND ii.invoice_at < sqlc.arg(cutoff)::timestamptz
WHERE s.merchant_id = $1
  AND s.billing_mode = 'arrears'
  AND s.credit_limit_amount > 0
GROUP BY s.merchant_id, s.customer_id, s.currency, s.credit_limit_amount
HAVING COALESCE(SUM(ii.amount), 0)::bigint + (
    SELECT COALESCE(SUM(i.amount_due), 0)::bigint
    FROM openrails.invoices i
    WHERE i.merchant_id = s.merchant_id
      AND i.customer_id = s.customer_id
      AND i.currency = s.currency
      AND i.status IN ('open', 'past_due')
      AND i.amount_due > 0
) >= CASE WHEN sqlc.arg(min_threshold)::bigint > 0 THEN sqlc.arg(min_threshold)::bigint ELSE s.credit_limit_amount END
ORDER BY period_from ASC;

-- name: ListChargeableOpenInvoices :many
SELECT i.id, i.merchant_id, i.customer_id, i.currency, i.amount_due,
       i.collection_failure_count, i.collection_failed_at,
       COALESCE(s.collection_payment_method_id, s.auto_topup_payment_method_id)::uuid AS collection_payment_method_id
FROM openrails.invoices i
JOIN openrails.money_settings s
  ON s.merchant_id = i.merchant_id
 AND s.customer_id = i.customer_id
 AND s.currency = i.currency
WHERE i.merchant_id = $1
  AND i.status IN ('open', 'past_due')
  AND i.amount_due > 0
  AND i.collection_method = 'charge_automatically'
  AND COALESCE(s.collection_payment_method_id, s.auto_topup_payment_method_id) IS NOT NULL
  AND i.last_collection_failure_code IS DISTINCT FROM 'collection_attempt_in_progress'
  AND i.last_collection_failure_code IS DISTINCT FROM 'collection_outcome_unknown'
  AND (i.due_at IS NULL OR i.due_at <= sqlc.arg(now)::timestamptz)
  AND (
      (i.collection_failure_count = 0 AND i.next_collection_attempt_at IS NULL)
      OR i.next_collection_attempt_at <= sqlc.arg(now)::timestamptz
  )
  AND (sqlc.arg(min_threshold)::bigint <= 0 OR i.amount_due >= sqlc.arg(min_threshold)::bigint)
ORDER BY i.due_at NULLS FIRST, i.created_at ASC;

-- name: ExpireStaleInvoiceCollectionClaims :execrows
UPDATE openrails.invoices
SET last_collection_failure_code = 'collection_outcome_unknown',
    last_collection_failure_message = NULL,
    next_collection_attempt_at = NULL,
    updated_at = sqlc.arg(now)::timestamptz
WHERE merchant_id = $1
  AND last_collection_failure_code = 'collection_attempt_in_progress'
  AND updated_at <= sqlc.arg(stale_before)::timestamptz;

-- name: RecordInvoiceCollectionFailure :execrows
UPDATE openrails.invoices
SET collection_failure_count = collection_failure_count + 1,
    collection_failed_at = COALESCE(collection_failed_at, sqlc.arg(now)::timestamptz),
    status = CASE WHEN sqlc.arg(terminal)::boolean THEN 'uncollectible' ELSE 'past_due' END,
    next_collection_attempt_at = sqlc.narg(next_attempt_at)::timestamptz,
    last_collection_failure_code = sqlc.narg(failure_code),
    last_collection_failure_message = sqlc.narg(failure_message),
    uncollectible_at = CASE WHEN sqlc.arg(terminal)::boolean THEN sqlc.arg(now)::timestamptz ELSE NULL END,
    updated_at = sqlc.arg(now)::timestamptz
WHERE merchant_id = $1
  AND customer_id = $2
  AND id = sqlc.arg(invoice_id)
  AND status IN ('open', 'past_due')
  AND collection_failure_count = sqlc.arg(expected_failure_count)::integer;

-- name: MarkInvoicesPastDue :execrows
-- #798: net-N receivables whose due date has passed become past_due. The
-- collection path already treats open and past_due alike; this transition is
-- the host-visible dunning signal.
UPDATE openrails.invoices
SET status = 'past_due',
    updated_at = sqlc.arg(now)::timestamptz
WHERE merchant_id = $1
  AND status = 'open'
  AND amount_due > 0
  AND due_at IS NOT NULL
  AND due_at < sqlc.arg(now)::timestamptz;

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
    payment_method_id, idempotency_key
) VALUES (
    $1, $2, $3, sqlc.arg(invoice_id), sqlc.narg(ledger_transfer_id),
    sqlc.arg(currency), sqlc.arg(amount), sqlc.arg(status), sqlc.narg(rail),
    sqlc.narg(rail_payment_id), sqlc.narg(failure_code), sqlc.narg(failure_reason), sqlc.narg(failure_message),
    sqlc.arg(attempted_at), sqlc.narg(settled_at), sqlc.arg(created_at), sqlc.arg(updated_at),
    sqlc.narg(payment_method_id), sqlc.narg(idempotency_key)
);

-- name: DeleteClaimedInvoicePaymentAttempt :execrows
DELETE FROM openrails.invoice_payments
WHERE merchant_id = $1
  AND customer_id = $2
  AND invoice_id = $3
  AND id = sqlc.arg(attempt_id)
  AND status = 'attempted';

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

-- name: SetInvoiceCollectionClaim :execrows
UPDATE openrails.invoices
SET status = 'past_due',
    next_collection_attempt_at = NULL,
    last_collection_failure_code = 'collection_attempt_in_progress',
    last_collection_failure_message = NULL,
    uncollectible_at = NULL,
    updated_at = sqlc.arg(now)::timestamptz
WHERE merchant_id = $1
  AND customer_id = $2
  AND id = sqlc.arg(invoice_id)
  AND status IN ('open', 'past_due', 'uncollectible')
  AND amount_due > 0
  AND last_collection_failure_code IS DISTINCT FROM 'collection_attempt_in_progress'
  AND last_collection_failure_code IS DISTINCT FROM 'collection_outcome_unknown'
;

-- name: MarkInvoiceCollectionOutcomeUnknown :execrows
UPDATE openrails.invoices
SET status = 'past_due',
    next_collection_attempt_at = NULL,
    last_collection_failure_code = 'collection_outcome_unknown',
    last_collection_failure_message = NULL,
    updated_at = sqlc.arg(now)::timestamptz
WHERE merchant_id = $1
  AND customer_id = $2
  AND id = sqlc.arg(invoice_id)
  AND status = 'past_due'
  AND amount_due > 0
  AND last_collection_failure_code = 'collection_attempt_in_progress';

-- name: ReleaseInvoiceCollectionRetry :execrows
UPDATE openrails.invoices
SET status = sqlc.arg(status)::text,
    next_collection_attempt_at = sqlc.narg(next_attempt_at)::timestamptz,
    last_collection_failure_code = sqlc.narg(failure_code)::text,
    last_collection_failure_message = sqlc.narg(failure_message)::text,
    uncollectible_at = sqlc.narg(uncollectible_at)::timestamptz,
    updated_at = sqlc.arg(now)::timestamptz
WHERE merchant_id = $1
  AND customer_id = $2
  AND id = sqlc.arg(invoice_id)
  AND status = 'past_due'
  AND last_collection_failure_code IN ('collection_attempt_in_progress', 'collection_outcome_unknown');

-- name: VoidInvoiceForPayer :one
UPDATE openrails.invoices
SET status = 'voided',
    amount_due = 0,
    voided_at = sqlc.arg(now)::timestamptz,
    updated_at = sqlc.arg(now)::timestamptz
WHERE merchant_id = $1 AND customer_id = $2 AND id = sqlc.arg(invoice_id)
  AND status IN ('draft', 'open', 'past_due')
  AND last_collection_failure_code IS DISTINCT FROM 'collection_attempt_in_progress'
RETURNING *;

-- name: MarkInvoiceUncollectibleForPayer :one
UPDATE openrails.invoices
SET status = 'uncollectible',
    uncollectible_at = sqlc.arg(now)::timestamptz,
    updated_at = sqlc.arg(now)::timestamptz
WHERE merchant_id = $1 AND customer_id = $2 AND id = sqlc.arg(invoice_id)
  AND status IN ('open', 'past_due')
  AND last_collection_failure_code IS DISTINCT FROM 'collection_attempt_in_progress'
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
