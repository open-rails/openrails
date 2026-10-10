-- billing.invoices: period invoices/statements. Arrears invoices become open
-- receivables at finalization; payments are allocated back to invoice_id.

-- name: ListInvoicePayers :many
-- Every (payer, currency) active since active_since that the period sweep must
-- finalize: payers with ledger money movement, and payers whose only activity
-- is catalog-priced usage that FinalizeInvoice still has to rate (no ledger row
-- exists before rating). A payer with no activity has nothing to invoice, so
-- the sweep scales with activity, not with every payer on file.
-- period_anchor is the payer's first recorded activity: its first transfer or
-- the opening of its balance account, which the first metered event opens.
-- Both are permanent, so anniversary windows never move when old usage
-- partitions are dropped.
WITH active AS (
    SELECT lt.customer_id, lt.currency
    FROM billing.ledger_transfers lt
    WHERE lt.merchant_id = sqlc.arg(merchant_id)::uuid AND lt.customer_id IS NOT NULL
      AND lt.created_at >= sqlc.arg(active_since)::timestamptz
    UNION
    SELECT ue.customer_id, ue.currency
    FROM billing.usage_events ue
    WHERE ue.merchant_id = sqlc.arg(merchant_id)::uuid AND ue.pricing_authority = 'catalog'
      AND ue.occurred_at >= sqlc.arg(active_since)::timestamptz
)
SELECT p.customer_id::uuid AS customer_id, p.currency,
       COALESCE(
           LEAST(
               (SELECT MIN(lt.created_at) FROM billing.ledger_transfers lt
                WHERE lt.merchant_id = sqlc.arg(merchant_id)::uuid AND lt.customer_id = p.customer_id AND lt.currency = p.currency),
               (SELECT a.created_at FROM billing.ledger_accounts a
                WHERE a.merchant_id = sqlc.arg(merchant_id)::uuid AND a.customer_id = p.customer_id
                  AND a.currency = p.currency AND a.account_type = 'customer_balance')),
           (SELECT MIN(ue.created_at) FROM billing.usage_events ue
            WHERE ue.merchant_id = sqlc.arg(merchant_id)::uuid AND ue.customer_id = p.customer_id AND ue.currency = p.currency
              AND ue.pricing_authority = 'catalog' AND ue.occurred_at >= sqlc.arg(active_since)::timestamptz)
       )::timestamptz AS period_anchor
FROM active p
ORDER BY p.customer_id, p.currency;

-- name: GetInvoiceByPeriod :one
-- Idempotency key is per (payer, period, currency): one invoice per currency (#474).
SELECT * FROM billing.invoices
WHERE merchant_id = $1 AND customer_id = $2
  AND period_starts_at = $3 AND period_ends_at = $4 AND currency = sqlc.arg(currency)
LIMIT 1;

-- name: InsertInvoice :exec
INSERT INTO billing.invoices (
    id, merchant_id, customer_id, currency,
    invoice_number,
    period_starts_at, period_ends_at, usage_total, deposits_total, owed_accrued, owed_paid,
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
INSERT INTO billing.invoice_items (
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
UPDATE billing.invoice_items
SET invoice_id = sqlc.arg(invoice_id),
    status = 'invoiced',
    updated_at = sqlc.arg(now)::timestamptz
WHERE merchant_id = $1
  AND customer_id = $2
  AND currency = sqlc.arg(currency)
  AND invoice_id IS NULL
  AND status = 'pending'
  AND invoice_at >= sqlc.arg(period_starts_at)::timestamptz
  AND invoice_at < sqlc.arg(period_ends_at)::timestamptz;

-- name: SumPendingInvoiceItemAmountInPeriod :one
SELECT COALESCE(SUM(amount), 0)::bigint
FROM billing.invoice_items
WHERE merchant_id = $1 AND customer_id = $2 AND currency = sqlc.arg(currency)
  AND invoice_id IS NULL AND status = 'pending'
  AND invoice_at >= sqlc.arg(period_starts_at)::timestamptz
  AND invoice_at < sqlc.arg(period_ends_at)::timestamptz;

-- name: SumBilledInvoiceItemAmountInPeriod :one
-- A period's charges that invoices already bill: what its statement waits on.
SELECT COALESCE(SUM(amount), 0)::bigint
FROM billing.invoice_items
WHERE merchant_id = $1 AND customer_id = $2 AND currency = sqlc.arg(currency)
  AND invoice_id IS NOT NULL
  AND invoice_at >= sqlc.arg(period_starts_at)::timestamptz
  AND invoice_at < sqlc.arg(period_ends_at)::timestamptz;

-- name: LockInvoicesBillingPeriod :many
-- The invoices billing a period's charges, share-locked while its statement
-- reads their payments: a payment to one commits first, or waits and then
-- finds the committed statement.
SELECT i.id
FROM billing.invoices i
WHERE i.merchant_id = $1 AND i.customer_id = $2
  AND i.id IN (
      SELECT ii.invoice_id FROM billing.invoice_items ii
      WHERE ii.merchant_id = $1 AND ii.customer_id = $2 AND ii.currency = sqlc.arg(currency)
        AND ii.invoice_id IS NOT NULL
        AND ii.invoice_at >= sqlc.arg(period_starts_at)::timestamptz
        AND ii.invoice_at < sqlc.arg(period_ends_at)::timestamptz)
ORDER BY i.id
FOR SHARE;

-- name: SumInvoiceItemAmountBilledBy :one
-- The charges an invoice bills itself; a statement's total adds the charges of
-- its period other invoices bill.
SELECT COALESCE(SUM(amount), 0)::bigint
FROM billing.invoice_items
WHERE merchant_id = $1 AND invoice_id = sqlc.arg(invoice_id)::uuid;

-- name: LockStatementsAwaitingInvoice :many
-- The statements waiting on an invoice's charges: their period holds one, and
-- not all they state is paid or due on them. Locked so payments to two of
-- their invoices recompute them one after the other.
WITH billed AS (
    SELECT MIN(invoice_at) AS first_at, MAX(invoice_at) AS last_at
    FROM billing.invoice_items
    WHERE merchant_id = $1 AND invoice_id = sqlc.arg(invoice_id)::uuid
)
SELECT s.id
FROM billing.invoices s, billed b
WHERE s.merchant_id = $1 AND s.customer_id = $2 AND s.currency = sqlc.arg(currency)
  AND s.status IN ('open', 'uncollectible')
  AND s.amount_paid + s.amount_due < s.total_amount
  AND s.id <> sqlc.arg(invoice_id)::uuid
  AND s.period_starts_at <= b.last_at AND s.period_ends_at > b.first_at
ORDER BY s.id
FOR UPDATE OF s;

-- name: SettleStatementCoverage :execrows
-- A statement's amount_paid is its own payments plus what the invoices billing
-- the rest of its period's charges received for them; each such invoice's
-- payments apply to its charges oldest first. The statement is paid once
-- nothing is due on it and those charges are all paid.
WITH s AS (
    SELECT i.id, i.customer_id, i.currency, i.period_starts_at, i.period_ends_at
    FROM billing.invoices i
    WHERE i.merchant_id = sqlc.arg(merchant_id)::uuid AND i.id = ANY(sqlc.arg(statement_ids)::uuid[])
      AND i.status IN ('open', 'uncollectible')
      AND i.amount_paid + i.amount_due < i.total_amount
), own AS (
    SELECT s.id, COALESCE(SUM(ii.amount), 0)::bigint AS amount
    FROM s
    LEFT JOIN billing.invoice_items ii ON ii.merchant_id = sqlc.arg(merchant_id)::uuid AND ii.invoice_id = s.id
    GROUP BY s.id
), coverage AS (
    SELECT s.id, ii.invoice_id, MIN(s.period_starts_at) AS period_starts_at, SUM(ii.amount)::bigint AS amount
    FROM s
    JOIN billing.invoice_items ii
      ON ii.merchant_id = sqlc.arg(merchant_id)::uuid AND ii.customer_id = s.customer_id AND ii.currency = s.currency
     AND ii.invoice_id IS NOT NULL AND ii.invoice_id <> s.id
     AND ii.invoice_at >= s.period_starts_at AND ii.invoice_at < s.period_ends_at
    GROUP BY s.id, ii.invoice_id
), received AS (
    SELECT c.id, SUM(LEAST(GREATEST(b.amount_paid - COALESCE(earlier.amount, 0), 0), c.amount))::bigint AS amount
    FROM coverage c
    JOIN billing.invoices b ON b.merchant_id = sqlc.arg(merchant_id)::uuid AND b.id = c.invoice_id
    LEFT JOIN LATERAL (
        SELECT SUM(e.amount) AS amount
        FROM billing.invoice_items e
        WHERE e.merchant_id = sqlc.arg(merchant_id)::uuid AND e.invoice_id = c.invoice_id
          AND e.invoice_at < c.period_starts_at
    ) earlier ON true
    GROUP BY c.id
), settled AS (
    SELECT own.id, own.amount AS own_amount, LEAST(COALESCE(received.amount, 0), i.total_amount - own.amount) AS received,
           i.amount_due = 0 AND COALESCE(received.amount, 0) >= i.total_amount - own.amount AS paid
    FROM own
    JOIN billing.invoices i ON i.merchant_id = sqlc.arg(merchant_id)::uuid AND i.id = own.id
    LEFT JOIN received ON received.id = own.id
)
UPDATE billing.invoices i
SET amount_paid = settled.own_amount - i.amount_due + settled.received,
    status = CASE WHEN settled.paid THEN 'paid' ELSE i.status END,
    paid_at = CASE WHEN settled.paid THEN sqlc.arg(now)::timestamptz ELSE i.paid_at END,
    updated_at = sqlc.arg(now)::timestamptz
FROM settled
WHERE i.merchant_id = sqlc.arg(merchant_id)::uuid AND i.id = settled.id;

-- name: ListInvoiceThresholdCandidates :many
--
-- or#897: the trigger amount is the BOUND billing policy's
-- collection_threshold_amount when the payer has one, else the merchant-wide
-- threshold, else the payer's own credit line. Resolved in SQL through the same
-- most-specific-wins rungs the admission path uses (money_settings.tier supplies
-- the tier rung), so a per-payer trigger costs no extra round trip.
SELECT s.customer_id, s.currency, MIN(ii.invoice_at)::timestamptz AS period_starts_at, MIN(s.created_at)::timestamptz AS period_anchor
FROM billing.money_settings s
JOIN billing.invoice_items ii
  ON ii.merchant_id = s.merchant_id
 AND ii.customer_id = s.customer_id
 AND ii.currency = s.currency
 AND ii.invoice_id IS NULL
 AND ii.status = 'pending'
 AND ii.invoice_at < sqlc.arg(cutoff)::timestamptz
LEFT JOIN LATERAL (
    SELECT (p.policy ->> 'collection_threshold_amount')::bigint AS threshold
    FROM billing.billing_policy_bindings b
    JOIN billing.billing_policies p
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
    FROM billing.invoices i
    WHERE i.merchant_id = s.merchant_id
      AND i.customer_id = s.customer_id
      AND i.currency = s.currency
      AND i.status = 'open'
      AND i.amount_due > 0
) >= COALESCE(
    pol.threshold,
    CASE WHEN sqlc.arg(min_threshold)::bigint > 0 THEN sqlc.arg(min_threshold)::bigint ELSE s.credit_limit_amount END)
ORDER BY period_starts_at ASC;

-- name: ListChargeableOpenInvoices :many
SELECT i.id, i.merchant_id, i.customer_id, i.currency, i.amount_due,
       i.collection_failure_count, i.collection_failed_at,
       s.default_payment_method_id::uuid AS default_payment_method_id
FROM billing.invoices i
JOIN billing.money_settings s
  ON s.merchant_id = i.merchant_id
 AND s.customer_id = i.customer_id
 AND s.currency = i.currency
WHERE i.merchant_id = $1
  AND i.status = 'open'
  AND i.amount_due > 0
  AND i.collection_method = 'charge_automatically'
  AND s.default_payment_method_id IS NOT NULL
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
-- Only a terminal outcome changes `status`. A decline bucket answers "what do
-- we do about the CARD"; being overdue is a reading of the CLOCK (due_at <
-- now) and belongs to the delinquency axis (or#878). The invoice stays open,
-- collectible and the customer's to settle.
UPDATE billing.invoices
SET collection_failure_count = collection_failure_count + 1,
    collection_failed_at = COALESCE(collection_failed_at, sqlc.arg(now)::timestamptz),
    status = CASE WHEN sqlc.arg(terminal)::boolean THEN 'uncollectible' ELSE status END,
    next_collection_attempt_at = sqlc.narg(next_attempt_at)::timestamptz,
    last_collection_failure_code = sqlc.narg(failure_code),
    last_collection_failure_message = sqlc.narg(failure_message),
    uncollectible_at = CASE WHEN sqlc.arg(terminal)::boolean THEN sqlc.arg(now)::timestamptz ELSE NULL END,
    collection_intent_id = NULL,
    updated_at = sqlc.arg(now)::timestamptz
WHERE merchant_id = $1
  AND customer_id = $2
  AND id = sqlc.arg(invoice_id)
  AND status = 'open'
  AND collection_intent_id = sqlc.arg(intent_id)::uuid;

-- name: StopInvoiceCollection :execrows
-- or#828 bucket 2 before any attempt (#1166): the collection card carries no
-- customer agreement for a merchant-initiated charge, so charging stops until
-- the customer acts, as a fix-your-card decline stops it.
UPDATE billing.invoices
SET collection_failure_count = collection_failure_count + 1,
    collection_failed_at = COALESCE(collection_failed_at, sqlc.arg(now)::timestamptz),
    next_collection_attempt_at = NULL,
    last_collection_failure_code = sqlc.arg(failure_code)::text,
    last_collection_failure_message = sqlc.arg(failure_message)::text,
    updated_at = sqlc.arg(now)::timestamptz
WHERE merchant_id = $1
  AND customer_id = $2
  AND id = sqlc.arg(invoice_id)::uuid
  AND status = 'open'
  AND collection_intent_id IS NULL;

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
UPDATE billing.invoices
SET next_collection_attempt_at = sqlc.arg(now)::timestamptz,
    updated_at = sqlc.arg(now)::timestamptz
WHERE merchant_id = $1
  AND customer_id = $2
  AND currency = sqlc.arg(currency)
  AND status = 'open'
  AND amount_due > 0
  AND collection_method = 'charge_automatically'
  AND collection_failure_count > 0
  AND next_collection_attempt_at IS NULL
  AND collection_intent_id IS NULL;

-- name: NotifyOverdueInvoices :one
-- Tells each payer once about each open invoice past its due date; the notice
-- id is the invoice's, so a later pass adds none.
WITH notices AS (
    INSERT INTO billing.notifications (id, merchant_id, customer_id, event_type, data, read_at, created_at)
    SELECT md5('invoice_overdue:' || id::text)::uuid, merchant_id, customer_id, 'invoice_overdue',
           jsonb_build_object('invoice_id', id,
                              'invoice_number', COALESCE(NULLIF(invoice_number, ''), id::text),
                              'amount_due', amount_due::text, 'currency', currency, 'due_at', due_at),
           NULL, sqlc.arg(now)::timestamptz
    FROM billing.invoices
    WHERE merchant_id = sqlc.arg(merchant_id)::uuid
      AND status = 'open' AND amount_due > 0
      AND due_at IS NOT NULL AND due_at < sqlc.arg(now)::timestamptz
    ON CONFLICT (merchant_id, id) DO NOTHING
    RETURNING id
)
SELECT count(*) FROM notices;

-- name: SumPendingInvoiceItemAmountBySourceInPeriod :many
-- #798: rated charge per accrual source for the statement's per-category
-- itemization at finalize. Metered accruals carry their rating identity
-- (e.g. metered:<meter>) in metadata->>'source' (the row source_id also
-- embeds the period window).
SELECT COALESCE(NULLIF(metadata ->> 'source', ''), source_id)::text AS source,
       COALESCE(SUM(amount), 0)::bigint AS amount,
       COUNT(*)::bigint AS item_count
FROM billing.invoice_items
WHERE merchant_id = $1 AND customer_id = $2 AND currency = sqlc.arg(currency)
  AND invoice_id IS NULL AND status = 'pending'
  AND invoice_at >= sqlc.arg(period_starts_at)::timestamptz
  AND invoice_at < sqlc.arg(period_ends_at)::timestamptz
GROUP BY 1
ORDER BY 1 ASC;

-- name: ListPendingInvoiceItemsByPayer :many
-- #798: current accrued-but-uninvoiced charges (running-spend surface).
SELECT source_type,
       COALESCE(NULLIF(metadata ->> 'source', ''), source_id)::text AS source,
       amount, invoice_at
FROM billing.invoice_items
WHERE merchant_id = $1 AND customer_id = $2 AND currency = sqlc.arg(currency)
  AND invoice_id IS NULL AND status = 'pending'
ORDER BY invoice_at ASC, source_id ASC;

-- name: ApplyInvoicePaymentSnapshot :execrows
-- Settling what is due leaves a statement open while other invoices still
-- owe part of its period.
UPDATE billing.invoices
SET amount_paid = amount_paid + sqlc.arg(snapshot)::bigint,
    amount_due = GREATEST(0, amount_due - sqlc.arg(snapshot)::bigint),
    status = CASE WHEN amount_due - sqlc.arg(snapshot)::bigint > 0 THEN status
                  WHEN amount_paid + sqlc.arg(snapshot)::bigint >= total_amount THEN 'paid'
                  ELSE 'open' END,
    paid_at = CASE WHEN amount_due - sqlc.arg(snapshot)::bigint <= 0 AND amount_paid + sqlc.arg(snapshot)::bigint >= total_amount
                   THEN sqlc.arg(now)::timestamptz ELSE paid_at END,
    next_collection_attempt_at = CASE WHEN amount_due - sqlc.arg(snapshot)::bigint <= 0 THEN NULL ELSE next_collection_attempt_at END,
    last_collection_failure_code = CASE WHEN amount_due - sqlc.arg(snapshot)::bigint <= 0 THEN NULL ELSE last_collection_failure_code END,
    last_collection_failure_message = CASE WHEN amount_due - sqlc.arg(snapshot)::bigint <= 0 THEN NULL ELSE last_collection_failure_message END,
    updated_at = sqlc.arg(now)::timestamptz
WHERE merchant_id = $1 AND customer_id = $2 AND id = sqlc.arg(invoice_id)
  AND status = 'open'
  AND amount_due >= sqlc.arg(snapshot)::bigint;

-- name: SetInvoiceExternalID :execrows
UPDATE billing.invoices
SET external_invoice_id = sqlc.arg(external_invoice_id),
    updated_at = sqlc.arg(now)::timestamptz
WHERE merchant_id = $1 AND customer_id = $2 AND id = sqlc.arg(invoice_id)
  AND (external_invoice_id IS NULL OR external_invoice_id = sqlc.arg(external_invoice_id));

-- name: InsertInvoicePayment :exec
-- A payment that paid an invoice: a settled collection charge or a recorded
-- remittance, with the owed-payment transfer that settled it.
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

-- name: ClaimInvoiceCollection :execrows
-- Points the invoice at its one live collection operation; reclaiming an
-- `uncollectible` invoice reopens it (a manual retry undoing a terminal
-- outcome). The previous failure code stays as forensics. Each claim counts
-- one collection attempt.
UPDATE billing.invoices
SET status = 'open',
    collection_attempt_count = collection_attempt_count + 1,
    next_collection_attempt_at = NULL,
    uncollectible_at = NULL,
    collection_intent_id = sqlc.arg(intent_id)::uuid,
    updated_at = sqlc.arg(now)::timestamptz
WHERE merchant_id = $1
  AND customer_id = $2
  AND id = sqlc.arg(invoice_id)
  AND status IN ('open', 'uncollectible')
  AND amount_due > 0
  AND collection_intent_id IS NULL;

-- name: ReleaseInvoiceCollection :execrows
-- The owning operation reached a terminal outcome that is not a decline
-- (settled, or provider-confirmed non-execution, which makes the invoice due
-- again at next_attempt_at).
UPDATE billing.invoices
SET collection_intent_id = NULL,
    next_collection_attempt_at = sqlc.narg(next_attempt_at)::timestamptz,
    updated_at = sqlc.arg(now)::timestamptz
WHERE merchant_id = $1
  AND customer_id = $2
  AND id = sqlc.arg(invoice_id)
  AND collection_intent_id = sqlc.arg(intent_id)::uuid;

-- name: VoidInvoiceForPayer :one
UPDATE billing.invoices
SET status = 'voided',
    amount_due = 0,
    voided_at = sqlc.arg(now)::timestamptz,
    updated_at = sqlc.arg(now)::timestamptz
WHERE merchant_id = $1 AND customer_id = $2 AND id = sqlc.arg(invoice_id)
  AND status IN ('draft', 'open')
  AND collection_intent_id IS NULL
RETURNING *;

-- name: MarkInvoiceUncollectibleForPayer :one
UPDATE billing.invoices
SET status = 'uncollectible',
    uncollectible_at = sqlc.arg(now)::timestamptz,
    updated_at = sqlc.arg(now)::timestamptz
WHERE merchant_id = $1 AND customer_id = $2 AND id = sqlc.arg(invoice_id)
  AND status = 'open'
  AND collection_intent_id IS NULL
RETURNING *;

-- name: GetInvoiceForPayer :one
SELECT * FROM billing.invoices
WHERE merchant_id = $1 AND customer_id = $2 AND id = $3
LIMIT 1;

-- name: GetInvoiceForPayerForUpdate :one
SELECT * FROM billing.invoices
WHERE merchant_id = $1 AND customer_id = $2 AND id = $3
LIMIT 1
FOR UPDATE;

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
WHERE i.merchant_id = sqlc.arg(merchant_id)::uuid AND i.intent_type = 'invoice_collection'
  AND (sqlc.narg(after_id)::uuid IS NULL OR i.id > sqlc.narg(after_id)::uuid)
ORDER BY i.id
LIMIT sqlc.arg(page_size)::int;

-- name: ListOwedInvoiceClaims :many
-- Invoices that still claim a customer's owed money, oldest first, locked so a
-- collection cannot start on one while funding repays it.
SELECT id, amount_due, collection_intent_id
FROM billing.invoices
WHERE merchant_id = $1 AND customer_id = $2 AND currency = sqlc.arg(currency)::text
  AND status IN ('open', 'uncollectible') AND amount_due > 0
ORDER BY due_at NULLS FIRST, created_at, id
FOR UPDATE;

-- name: SumOwedInvoiceClaims :one
SELECT COALESCE(SUM(amount_due), 0)::bigint AS amount
FROM billing.invoices
WHERE merchant_id = $1 AND customer_id = $2 AND currency = sqlc.arg(currency)::text
  AND status IN ('open', 'uncollectible') AND amount_due > 0;

-- name: ApplyInvoiceBalancePayment :execrows
-- Pays an invoice from the customer's funded balance. An invoice with a
-- collection in flight keeps its claim.
UPDATE billing.invoices
SET amount_paid = amount_paid + sqlc.arg(amount)::bigint,
    amount_due = amount_due - sqlc.arg(amount)::bigint,
    status = CASE WHEN amount_due <> sqlc.arg(amount)::bigint THEN status
                  WHEN amount_paid + sqlc.arg(amount)::bigint >= total_amount THEN 'paid'
                  ELSE 'open' END,
    paid_at = CASE WHEN amount_due = sqlc.arg(amount)::bigint AND amount_paid + sqlc.arg(amount)::bigint >= total_amount
                   THEN sqlc.arg(now)::timestamptz ELSE paid_at END,
    next_collection_attempt_at = CASE WHEN amount_due = sqlc.arg(amount)::bigint THEN NULL ELSE next_collection_attempt_at END,
    updated_at = sqlc.arg(now)::timestamptz
WHERE merchant_id = $1 AND customer_id = $2 AND id = sqlc.arg(invoice_id)
  AND status IN ('open', 'uncollectible')
  AND collection_intent_id IS NULL
  AND amount_due >= sqlc.arg(amount)::bigint;
