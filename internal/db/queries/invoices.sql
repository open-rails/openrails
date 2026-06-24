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
    issued_at, due_at, paid_at, voided_at, uncollectible_at, sent_at,
    finalized_at, external_invoice_id, created_at, updated_at
) VALUES (
    $1, $2, $3, $4,
    sqlc.narg(invoice_number),
    $5, $6, $7, $8, $9, $10,
    $11, sqlc.arg(subtotal_amount), sqlc.arg(total_amount), sqlc.arg(amount_paid), sqlc.arg(amount_due),
    COALESCE(sqlc.arg(line_items), '[]'::jsonb), COALESCE(sqlc.arg(money_movements), '{}'::jsonb),
    sqlc.arg(status), sqlc.arg(collection_method),
    sqlc.narg(issued_at), sqlc.narg(due_at), sqlc.narg(paid_at), sqlc.narg(voided_at),
    sqlc.narg(uncollectible_at), sqlc.narg(sent_at), sqlc.narg(finalized_at),
    sqlc.narg(external_invoice_id), sqlc.arg(created_at), sqlc.arg(updated_at)
);

-- name: InsertInvoiceItem :exec
INSERT INTO openrails.invoice_items (
    id, merchant_id, customer_id, currency, invoice_id,
    source_type, source_id, event_type,
    period_from, period_to, invoice_at,
    quantity, unit_amount, amount, status, metadata,
    created_at, updated_at
) VALUES (
    $1, $2, $3, sqlc.arg(currency), sqlc.narg(invoice_id),
    sqlc.arg(source_type), sqlc.arg(source_id), sqlc.narg(event_type),
    sqlc.arg(period_from), sqlc.arg(period_to), sqlc.arg(invoice_at),
    sqlc.arg(quantity), sqlc.arg(unit_amount), sqlc.arg(amount), sqlc.arg(status),
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
SELECT s.customer_id, s.currency, MIN(ii.invoice_at)::timestamptz AS period_from
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
) >= s.credit_limit_amount
ORDER BY period_from ASC;

-- name: ListChargeableOpenInvoices :many
SELECT i.id, i.merchant_id, i.customer_id, i.currency, i.amount_due, s.auto_topup_payment_method_id
FROM openrails.invoices i
JOIN openrails.money_settings s
  ON s.merchant_id = i.merchant_id
 AND s.customer_id = i.customer_id
 AND s.currency = i.currency
WHERE i.merchant_id = $1
  AND i.status IN ('open', 'past_due')
  AND i.amount_due > 0
  AND s.auto_topup_payment_method_id IS NOT NULL
  AND (sqlc.arg(min_threshold)::bigint <= 0 OR i.amount_due >= sqlc.arg(min_threshold)::bigint)
ORDER BY i.due_at NULLS FIRST, i.created_at ASC;

-- name: ApplyInvoicePaymentSnapshot :execrows
UPDATE openrails.invoices
SET amount_paid = amount_paid + sqlc.arg(snapshot)::bigint,
    amount_due = GREATEST(0, amount_due - sqlc.arg(snapshot)::bigint),
    status = CASE WHEN amount_due - sqlc.arg(snapshot)::bigint <= 0 THEN 'paid' ELSE status END,
    paid_at = CASE WHEN amount_due - sqlc.arg(snapshot)::bigint <= 0 THEN sqlc.arg(now)::timestamptz ELSE paid_at END,
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
    id, merchant_id, customer_id, invoice_id, money_transaction_id,
    currency, amount, status, rail, rail_payment_id,
    failure_code, failure_message, attempted_at, settled_at, created_at, updated_at
) VALUES (
    $1, $2, $3, sqlc.arg(invoice_id), sqlc.narg(money_transaction_id),
    sqlc.arg(currency), sqlc.arg(amount), sqlc.arg(status), sqlc.narg(rail),
    sqlc.narg(rail_payment_id), sqlc.narg(failure_code), sqlc.narg(failure_message),
    sqlc.arg(attempted_at), sqlc.narg(settled_at), sqlc.arg(created_at), sqlc.arg(updated_at)
);

-- name: VoidInvoiceForPayer :one
UPDATE openrails.invoices
SET status = 'voided',
    amount_due = 0,
    voided_at = sqlc.arg(now)::timestamptz,
    updated_at = sqlc.arg(now)::timestamptz
WHERE merchant_id = $1 AND customer_id = $2 AND id = sqlc.arg(invoice_id)
  AND status IN ('draft', 'open', 'past_due')
RETURNING *;

-- name: MarkInvoiceUncollectibleForPayer :one
UPDATE openrails.invoices
SET status = 'uncollectible',
    uncollectible_at = sqlc.arg(now)::timestamptz,
    updated_at = sqlc.arg(now)::timestamptz
WHERE merchant_id = $1 AND customer_id = $2 AND id = sqlc.arg(invoice_id)
  AND status IN ('open', 'past_due')
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
