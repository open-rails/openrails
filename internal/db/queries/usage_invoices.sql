-- Usage invoices (invoices.origin = 'usage'): owed charges billed in arrears for
-- a period. Pending charges wait in owed_items until an invoice bills them.

-- name: GetUsageInvoiceOfWindow :one
-- The usage invoice a close of the window [window_starts_at, period_ends_at)
-- made: it ends with the window and starts no later, where its charges began.
SELECT * FROM billing.invoices
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND customer_id = sqlc.arg(customer_id)::uuid AND origin = 'usage'
  AND currency = sqlc.arg(currency)::text
  AND period_ends_at = sqlc.arg(period_ends_at)::timestamptz AND period_starts_at <= sqlc.arg(window_starts_at)::timestamptz
ORDER BY period_starts_at DESC
LIMIT 1;

-- name: EarliestPendingOwedCharge :one
-- Where the charges of a window's pending owed items began: each item's
-- period start, else when it accrued; window_starts_at when none is pending.
SELECT COALESCE(MIN(LEAST(accrued_at, COALESCE((metadata ->> 'period_starts_at')::timestamptz, accrued_at))),
    sqlc.arg(window_starts_at)::timestamptz)::timestamptz AS starts_at
FROM billing.owed_items
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND customer_id = sqlc.arg(customer_id)::uuid AND currency = sqlc.arg(currency)::text
  AND invoice_id IS NULL AND status = 'pending'
  AND accrued_at >= sqlc.arg(window_starts_at)::timestamptz
  AND accrued_at < sqlc.arg(window_ends_at)::timestamptz;

-- name: CreateUsageInvoice :exec
-- An open usage invoice, first charged at its due date when it is collected
-- automatically; one a funded balance already paid in full is created
-- complete, numbered.
INSERT INTO billing.invoices (
    merchant_id, id, customer_id, origin, status, payment_status, currency, total, amount_paid, number,
    period_starts_at, period_ends_at, due_at, completed_at, collection_method, po_number, memo, tax, billing_contacts,
    collection_failure_count, next_collection_attempt_at, created_at, updated_at
) VALUES (
    sqlc.arg(merchant_id)::uuid, sqlc.arg(id)::uuid, sqlc.arg(customer_id)::uuid, 'usage',
    CASE WHEN sqlc.narg(number)::bigint IS NULL THEN 'open' ELSE 'complete' END,
    CASE WHEN sqlc.narg(number)::bigint IS NULL THEN 'requires_payment_method' ELSE 'succeeded' END,
    sqlc.arg(currency)::text, sqlc.arg(total)::bigint, sqlc.arg(amount_paid)::bigint, sqlc.narg(number)::bigint,
    sqlc.arg(period_starts_at)::timestamptz, sqlc.arg(period_ends_at)::timestamptz, sqlc.arg(due_at)::timestamptz,
    CASE WHEN sqlc.narg(number)::bigint IS NULL THEN NULL ELSE sqlc.arg(now)::timestamptz END,
    sqlc.arg(collection_method)::text, sqlc.narg(po_number)::text, sqlc.narg(memo)::text, sqlc.arg(tax)::jsonb, sqlc.arg(billing_contacts)::jsonb,
    0, CASE WHEN sqlc.narg(number)::bigint IS NULL AND sqlc.arg(collection_method)::text = 'charge_automatically' THEN sqlc.arg(due_at)::timestamptz END,
    sqlc.arg(now)::timestamptz, sqlc.arg(now)::timestamptz
);

-- name: CreateUsageInvoiceLine :exec
INSERT INTO billing.invoice_lines (merchant_id, id, invoice_id, customer_id, "position", description, unit_amount, amount, source, created_at)
VALUES (sqlc.arg(merchant_id)::uuid, sqlc.arg(id)::uuid, sqlc.arg(invoice_id)::uuid, sqlc.arg(customer_id)::uuid, sqlc.arg(position)::int,
        sqlc.arg(source)::text, sqlc.arg(amount)::bigint, sqlc.arg(amount)::bigint, sqlc.arg(source)::text, sqlc.arg(now)::timestamptz);

-- name: InsertPendingOwedItem :exec
-- owed_items is the pending-accrual workspace: rows are born pending and only
-- leave via AttachPendingOwedItemsToInvoice.
INSERT INTO billing.owed_items (
    id, merchant_id, customer_id, currency,
    source_type, source_id, accrued_at, amount, metadata,
    created_at, updated_at
) VALUES (
    sqlc.arg(id)::uuid, sqlc.arg(merchant_id)::uuid, sqlc.arg(customer_id)::uuid, sqlc.arg(currency),
    sqlc.arg(source_type), sqlc.arg(source_id), sqlc.arg(accrued_at), sqlc.arg(amount),
    COALESCE(sqlc.arg(metadata), '{}'::jsonb),
    sqlc.arg(created_at), sqlc.arg(updated_at)
)
ON CONFLICT (merchant_id, customer_id, currency, source_type, source_id) DO NOTHING;

-- name: AttachPendingOwedItemsToInvoice :execrows
UPDATE billing.owed_items
SET invoice_id = sqlc.arg(invoice_id)::uuid,
    status = 'billed',
    updated_at = sqlc.arg(now)::timestamptz
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND customer_id = sqlc.arg(customer_id)::uuid
  AND currency = sqlc.arg(currency)
  AND invoice_id IS NULL
  AND status = 'pending'
  AND accrued_at >= sqlc.arg(period_starts_at)::timestamptz
  AND accrued_at < sqlc.arg(period_ends_at)::timestamptz;

-- name: SumPendingOwedItemAmountBySourceInPeriod :many
-- A period's pending charges per accrual source: a usage invoice's lines.
-- Metered accruals carry their rating identity (e.g. metered:<meter>) in
-- metadata->>'source'.
SELECT COALESCE(NULLIF(metadata ->> 'source', ''), source_type)::text AS source,
       COALESCE(SUM(amount), 0)::bigint AS amount,
       COUNT(*)::bigint AS item_count
FROM billing.owed_items
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND customer_id = sqlc.arg(customer_id)::uuid AND currency = sqlc.arg(currency)
  AND invoice_id IS NULL AND status = 'pending'
  AND accrued_at >= sqlc.arg(period_starts_at)::timestamptz
  AND accrued_at < sqlc.arg(period_ends_at)::timestamptz
GROUP BY 1
ORDER BY 1 ASC;

-- name: ListDueUsageCollections :many
-- CROSS-MERCHANT: open usage invoices collected automatically whose next
-- charge is due, in due order; none of a merchant whose rebills are held.
-- An idle pass reads no row.
SELECT merchant_id, id, customer_id, next_collection_attempt_at::timestamptz AS next_collection_attempt_at
FROM billing.invoices
WHERE origin = 'usage' AND status = 'open' AND collection_method = 'charge_automatically'
  AND next_collection_attempt_at <= sqlc.arg(now)::timestamptz
  AND NOT EXISTS (SELECT 1 FROM billing.merchant_controls c WHERE c.merchant_id = invoices.merchant_id AND c.rebills_held)
  AND (sqlc.narg(after_at)::timestamptz IS NULL
       OR (next_collection_attempt_at, merchant_id, id) > (sqlc.narg(after_at)::timestamptz, sqlc.narg(after_merchant_id)::uuid, sqlc.narg(after_id)::uuid))
ORDER BY next_collection_attempt_at, merchant_id, id
LIMIT sqlc.arg(row_limit)::int;

-- name: ParkUsageCollection :execrows
-- The customer has no card that can pay the invoice: charging waits until it
-- designates one.
UPDATE billing.invoices SET next_collection_attempt_at = NULL, updated_at = sqlc.arg(now)::timestamptz
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid AND origin = 'usage' AND status = 'open';

-- name: LockUsageInvoice :one
SELECT * FROM billing.invoices
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND customer_id = sqlc.arg(customer_id)::uuid AND id = sqlc.arg(id)::uuid AND origin = 'usage'
FOR UPDATE;

-- name: StartUsageCollection :execrows
-- Points the usage invoice at its one live collection, its attempt. Collecting
-- an uncollectible invoice reopens it.
UPDATE billing.invoices
SET status = 'processing', payment_status = 'processing', attempt_id = sqlc.arg(attempt_id)::uuid,
    payment_method_id = sqlc.arg(payment_method_id)::uuid, psp_id = sqlc.arg(psp_id)::uuid,
    next_collection_attempt_at = NULL, uncollectible_at = NULL, updated_at = sqlc.arg(now)::timestamptz
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid AND origin = 'usage'
  AND status IN ('open', 'uncollectible') AND amount_paid < total;

-- name: EndUsageCollection :execrows
-- A collection that moved no money, settled, or was not executed: the invoice
-- takes payment again, its next scheduled try at next_attempt_at (NULL: none
-- scheduled).
UPDATE billing.invoices
SET status = 'open', payment_status = 'requires_payment_method',
    next_collection_attempt_at = sqlc.narg(next_attempt_at)::timestamptz, updated_at = sqlc.arg(now)::timestamptz
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid AND origin = 'usage'
  AND status = 'processing' AND attempt_id = sqlc.arg(attempt_id)::uuid;

-- name: RecordUsageCollectionFailure :execrows
-- A declined collection, in three dispositions encoded by the arguments:
-- terminal (uncollectible), no next attempt (charging stops until the
-- customer fixes their card), or a next attempt (still dunning). Being
-- overdue is a reading of the clock, the delinquency axis; only a terminal
-- outcome closes collection.
UPDATE billing.invoices
SET status = CASE WHEN sqlc.arg(terminal)::boolean THEN 'uncollectible' ELSE 'open' END,
    payment_status = 'requires_payment_method',
    collection_failure_count = collection_failure_count + 1,
    collection_failed_at = COALESCE(collection_failed_at, sqlc.arg(now)::timestamptz),
    next_collection_attempt_at = sqlc.narg(next_attempt_at)::timestamptz,
    last_payment_error = sqlc.arg(last_payment_error)::jsonb,
    uncollectible_at = CASE WHEN sqlc.arg(terminal)::boolean THEN sqlc.arg(now)::timestamptz END,
    updated_at = sqlc.arg(now)::timestamptz
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid AND origin = 'usage'
  AND status = 'processing' AND attempt_id = sqlc.arg(attempt_id)::uuid;

-- name: StopUsageCollection :execrows
-- The collection card carries no customer agreement for a merchant-initiated
-- charge: charging stops until the customer acts, as a fix-your-card decline
-- stops it.
UPDATE billing.invoices
SET collection_failure_count = collection_failure_count + 1,
    collection_failed_at = COALESCE(collection_failed_at, sqlc.arg(now)::timestamptz),
    next_collection_attempt_at = NULL,
    last_payment_error = sqlc.arg(last_payment_error)::jsonb,
    updated_at = sqlc.arg(now)::timestamptz
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid AND origin = 'usage' AND status = 'open';

-- name: ResumeStoppedUsageCollection :execrows
-- A customer designating a collection card did what a stopped or parked
-- collection waited for: its usage invoices are due again at once.
-- Uncollectible ones need the merchant, and live collections belong to their
-- verifier.
UPDATE billing.invoices
SET next_collection_attempt_at = GREATEST(sqlc.arg(now)::timestamptz, due_at),
    updated_at = sqlc.arg(now)::timestamptz
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND customer_id = sqlc.arg(customer_id)::uuid AND currency = sqlc.arg(currency)
  AND origin = 'usage' AND status = 'open' AND amount_paid < total
  AND collection_method = 'charge_automatically'
  AND next_collection_attempt_at IS NULL;

-- name: EarliestPendingOwedAccrual :one
-- When the customer's unbilled usage in the currency began; none for none.
SELECT accrued_at FROM billing.owed_items
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND customer_id = sqlc.arg(customer_id)::uuid AND currency = sqlc.arg(currency)::text
  AND invoice_id IS NULL AND status = 'pending'
ORDER BY accrued_at
LIMIT 1;

-- name: ListOpenUsageInvoicesOfCurrency :many
-- The customer's open usage invoices in the currency, oldest first: at most a
-- few, each a closed billing window.
SELECT * FROM billing.invoices
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND customer_id = sqlc.arg(customer_id)::uuid AND currency = sqlc.arg(currency)::text
  AND origin = 'usage' AND status = 'open' AND amount_paid < total
ORDER BY created_at, id
LIMIT sqlc.arg(row_limit)::int;

-- name: PayUsageInvoice :one
-- Applies a payment, remittance or balance repayment to a usage invoice. A live
-- collection keeps its claim: only its own settlement pays the invoice then.
UPDATE billing.invoices
SET amount_paid = amount_paid + sqlc.arg(amount)::bigint,
    next_collection_attempt_at = CASE WHEN amount_paid + sqlc.arg(amount)::bigint = total THEN NULL ELSE next_collection_attempt_at END,
    last_payment_error = CASE WHEN amount_paid + sqlc.arg(amount)::bigint = total THEN NULL ELSE last_payment_error END,
    updated_at = sqlc.arg(now)::timestamptz
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid AND origin = 'usage'
  AND (status IN ('open', 'uncollectible') OR (status = 'processing' AND attempt_id = sqlc.narg(attempt_id)::uuid))
  AND amount_paid + sqlc.arg(amount)::bigint <= total
RETURNING *;

-- name: CompleteUsageInvoice :execrows
-- A usage invoice paid in full: numbered and complete. payment_id is the
-- payment that completed it, NULL when a funded balance did.
UPDATE billing.invoices
SET status = 'complete', payment_status = 'succeeded', number = sqlc.arg(number)::bigint,
    payment_id = COALESCE(sqlc.narg(payment_id)::uuid, payment_id), completed_at = sqlc.arg(now)::timestamptz,
    uncollectible_at = NULL, updated_at = sqlc.arg(now)::timestamptz
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid AND origin = 'usage'
  AND status IN ('open', 'processing', 'uncollectible') AND amount_paid = total;

-- name: VoidUsageInvoice :execrows
UPDATE billing.invoices
SET status = 'void', payment_status = 'requires_payment_method', voided_at = sqlc.arg(now)::timestamptz,
    next_collection_attempt_at = NULL, uncollectible_at = NULL, updated_at = sqlc.arg(now)::timestamptz
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid AND origin = 'usage'
  AND status IN ('open', 'uncollectible');

-- name: MarkUsageInvoiceUncollectible :execrows
UPDATE billing.invoices
SET status = 'uncollectible', uncollectible_at = sqlc.arg(now)::timestamptz, next_collection_attempt_at = NULL,
    updated_at = sqlc.arg(now)::timestamptz
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid AND origin = 'usage'
  AND status = 'open' AND amount_paid < total;

-- name: ListOwedInvoiceClaims :many
-- Usage invoices that still claim a customer's owed money, oldest first, locked
-- so a collection cannot start on one while funding repays it. A processing
-- one has a collection in flight.
SELECT id, total - amount_paid AS amount_due, status
FROM billing.invoices
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND customer_id = sqlc.arg(customer_id)::uuid AND currency = sqlc.arg(currency)::text
  AND origin = 'usage' AND status IN ('open', 'processing', 'uncollectible') AND amount_paid < total
ORDER BY due_at, created_at, id
FOR UPDATE;

-- name: SumOwedInvoiceClaims :one
SELECT COALESCE(SUM(total - amount_paid), 0)::bigint AS amount
FROM billing.invoices
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND customer_id = sqlc.arg(customer_id)::uuid AND currency = sqlc.arg(currency)::text
  AND origin = 'usage' AND status IN ('open', 'processing', 'uncollectible') AND amount_paid < total;

-- name: CountInvoiceAttempts :one
-- An invoice's attempts so far: a scheduled collection's ordinal.
SELECT count(*)::bigint FROM billing.invoice_attempts
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND invoice_id = sqlc.arg(invoice_id)::uuid;

-- name: InsertInvoicePayment :exec
-- A payment that paid a usage invoice: a settled collection charge or a
-- recorded remittance, with the owed-payment transfer that settled it.
INSERT INTO billing.payments (
    id, merchant_id, customer_id, invoice_id, ledger_transfer_id, channel, rail, psp_id, transaction_id,
    amount, list_amount, currency, status, purchased_at, created_at, money_movement, token_type
) VALUES (
    sqlc.arg(id)::uuid, sqlc.arg(merchant_id)::uuid, sqlc.arg(customer_id)::uuid, sqlc.arg(invoice_id)::uuid, sqlc.arg(ledger_transfer_id)::uuid,
    sqlc.arg(channel)::text, sqlc.narg(rail)::text, sqlc.narg(psp_id)::uuid, sqlc.arg(transaction_id)::text,
    sqlc.arg(amount)::bigint, sqlc.arg(amount)::bigint, sqlc.arg(currency)::text, 'succeeded', sqlc.arg(paid_at)::timestamptz, sqlc.arg(now)::timestamptz,
    CASE WHEN sqlc.arg(channel)::text = 'rail' THEN 'rail' ELSE 'none' END, sqlc.narg(token_type)::text
);

-- name: GetInvoicePayment :one
SELECT * FROM billing.payments
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND customer_id = sqlc.arg(customer_id)::uuid
  AND invoice_id = sqlc.arg(invoice_id)::uuid AND id = sqlc.arg(id)::uuid;

-- name: ListInvoiceCollectionsForArchive :many
-- Every terminal invoice collection with the payment its payload names, if
-- any, and that payment's ledger transfer: the archive checks each pair
-- before exporting or restoring it.
SELECT sqlc.embed(i), a.id AS payment_id, a.customer_id AS payment_customer_id, a.invoice_id AS payment_invoice_id,
    a.psp_id AS payment_psp_id, a.rail AS payment_rail, a.transaction_id AS payment_transaction_id, a.amount AS payment_amount,
    a.currency AS payment_currency, a.status AS payment_status, l.amount AS ledger_amount,
    COALESCE(l.merchant_id = a.merchant_id AND l.customer_id = a.customer_id
        AND l.invoice_id = a.invoice_id AND l.currency = a.currency
        AND l.source = 'invoice_charge' AND l.source_id = i.idempotency_key
        AND l.operation = 'invoice_payment' AND l.transfer_type = 'owed_payment', false)::boolean AS ledger_matches
FROM billing.provider_intents i
LEFT JOIN billing.payments a ON a.merchant_id = i.merchant_id AND a.id = (i.payload->>'payment_id')::uuid
LEFT JOIN billing.ledger_transfers l ON l.merchant_id = a.merchant_id AND l.id = a.ledger_transfer_id
WHERE i.merchant_id = sqlc.arg(merchant_id)::uuid AND i.intent_type = 'order_collection'
  AND (sqlc.narg(after_id)::uuid IS NULL OR i.id > sqlc.narg(after_id)::uuid)
ORDER BY i.id
LIMIT sqlc.arg(page_size)::int;

-- name: GetUsageRecoveryPayment :many
SELECT p.* FROM billing.payments p
JOIN billing.invoices o ON o.merchant_id = p.merchant_id AND o.id = p.invoice_id AND o.origin = 'usage'
WHERE p.merchant_id = sqlc.arg(merchant_id)::uuid AND p.psp_id = sqlc.arg(psp_id)::uuid
  AND p.transaction_id = sqlc.arg(transaction_id)::text
LIMIT 2;

-- name: UsageRecoveryHasOtherPayment :one
-- A transaction already allocated to a subscription or purchase cannot also
-- settle a usage invoice, including soft-deleted financial rows.
SELECT EXISTS(SELECT 1 FROM billing.payments p
    WHERE p.merchant_id = sqlc.arg(merchant_id)::uuid AND p.psp_id = sqlc.arg(psp_id)::uuid AND p.transaction_id = sqlc.arg(transaction_id)::text
      AND NOT EXISTS (SELECT 1 FROM billing.invoices o WHERE o.merchant_id = p.merchant_id AND o.id = p.invoice_id AND o.origin = 'usage'))::boolean;

-- name: UsageRecoveryVaultCustomers :many
SELECT DISTINCT customer_id FROM billing.payment_methods
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND psp_id = sqlc.arg(psp_id)::uuid
  AND rail = 'nmi' AND rail_customer_ref = sqlc.arg(vault)::text
LIMIT 2;

-- name: UsageRecoveryAllocationTotal :one
-- The invoice's retained paid amount must be explained by its allocations,
-- including native rounding (provider amount can exceed the allocated debt).
SELECT COALESCE(sum(l.amount), 0)::bigint AS amount,
  COALESCE(bool_and(l.id IS NOT NULL AND l.invoice_id IS NOT DISTINCT FROM p.invoice_id AND l.customer_id IS NOT DISTINCT FROM p.customer_id
    AND l.currency = p.currency AND l.currency = sqlc.arg(currency)::text
    AND l.transfer_type = 'owed_payment' AND l.operation IN ('invoice_payment', 'manual_invoice_payment')
    AND l.amount > 0 AND p.amount >= l.amount), true)::boolean AS consistent
FROM billing.payments p LEFT JOIN billing.ledger_transfers l
  ON l.merchant_id = p.merchant_id AND l.id = p.ledger_transfer_id
WHERE p.merchant_id = sqlc.arg(merchant_id)::uuid AND p.invoice_id = sqlc.arg(invoice_id)::uuid AND p.status = 'succeeded' AND p.deleted_at IS NULL;

-- name: GetUsageRecoveryLedgerTransfer :one
SELECT * FROM billing.ledger_transfers
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(transfer_id)::uuid;

-- name: UsageRecoveryAccounts :many
-- A different chosen card does not hide a prior account's paid invoice, even
-- if that card was added after the backup. Inspect every known card account
-- in this deployment's money class; unrelated rails cannot collect it.
SELECT p.id, p.rail FROM billing.psps p
WHERE p.merchant_id = sqlc.arg(merchant_id)::uuid AND p.rail IN ('nmi', 'stripe')
  AND p.live = (SELECT live FROM billing.psps WHERE merchant_id = p.merchant_id AND id = sqlc.arg(routed_psp)::uuid)
ORDER BY p.id;
