-- name: CreateRevenueSchedule :one
-- A paid line's schedule, at its application, as its first recognition left
-- it. Once per line and currency: a replay inserts nothing and returns no
-- row.
INSERT INTO billing.revenue_schedules (
    merchant_id, id, customer_id, currency, invoice_id, invoice_line_id, subscription_id, product_access_id,
    product_id, price_id, method, revenue_account, funded, recognized_net,
    service_starts_at, service_ends_at, service_months, ended_at, recognized_through_at, next_recognition_at, reinstatements, forfeited
) VALUES (
    sqlc.arg(merchant_id)::uuid, sqlc.arg(id)::uuid, sqlc.arg(customer_id)::uuid, sqlc.arg(currency)::text,
    sqlc.arg(invoice_id)::uuid, sqlc.arg(invoice_line_id)::uuid, sqlc.narg(subscription_id)::uuid, sqlc.narg(product_access_id)::uuid,
    sqlc.narg(product_id)::uuid, sqlc.narg(price_id)::uuid, sqlc.arg(method)::text, sqlc.arg(revenue_account)::text,
    sqlc.arg(funded)::bigint, sqlc.arg(recognized_net)::bigint,
    sqlc.arg(service_starts_at)::timestamptz, sqlc.arg(service_ends_at)::timestamptz, sqlc.narg(service_months)::int,
    sqlc.narg(ended_at)::timestamptz, sqlc.arg(recognized_through_at)::timestamptz, sqlc.narg(next_recognition_at)::timestamptz, 0,
    sqlc.arg(forfeited)::bigint
)
ON CONFLICT (merchant_id, invoice_line_id, currency) DO NOTHING
RETURNING *;

-- name: LockDueRevenueSchedule :one
-- A schedule still due at now, locked for its recognition.
SELECT * FROM billing.revenue_schedules
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid
  AND next_recognition_at <= sqlc.arg(now)::timestamptz
FOR UPDATE;

-- name: SetRevenueScheduleRecognized :execrows
UPDATE billing.revenue_schedules
SET recognized_net = sqlc.arg(recognized_net)::bigint,
    recognized_through_at = sqlc.arg(recognized_through_at)::timestamptz,
    next_recognition_at = sqlc.narg(next_recognition_at)::timestamptz,
    reinstatements = sqlc.arg(reinstatements)::int,
    forfeited = sqlc.arg(forfeited)::bigint,
    updated_at = now()
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid;

-- name: ListCreditLineReleases :many
-- What spends of credit released into an invoice's lines priced in a merchant's
-- own unit: per line and paid currency, the lots' money credited to
-- deferred_revenue under the schedule the spends named, and the latest release's
-- instant.
SELECT t.invoice_line_id::uuid AS invoice_line_id, t.currency, t.revenue_schedule_id::uuid AS revenue_schedule_id, sum(t.amount)::bigint AS amount,
    max(t.effective_at)::timestamptz AS released_at
FROM billing.ledger_transfers t
JOIN billing.ledger_accounts c ON c.merchant_id = t.merchant_id AND c.id = t.credit_account_id
WHERE t.merchant_id = sqlc.arg(merchant_id)::uuid AND t.source = 'order_line' AND t.source_id = ANY(sqlc.arg(invoice_line_ids)::text[])
  AND t.book = 'general' AND t.credit_grant_id IS NOT NULL AND t.revenue_schedule_id IS NOT NULL
  AND c.account_type = 'deferred_revenue'
GROUP BY t.invoice_line_id, t.currency, t.revenue_schedule_id
ORDER BY t.invoice_line_id, t.currency;

-- name: ListCreditLotRevenue :many
-- What a lot's money funds now, per revenue schedule its spends paid and its
-- invoice line (NULL: usage revenue): what the spends released, less what the
-- lot's reversals took back of it, plus what recoveries restored.
SELECT t.revenue_schedule_id, t.invoice_id, t.invoice_line_id, sum(CASE
    WHEN t.transfer_type = 'credit_released' THEN t.amount
    WHEN t.operation = 'purchased_credit_refund' AND t.source = 'money_recognized' THEN -t.amount
    WHEN t.source LIKE 'money\_recognized\_%restore:%' THEN t.amount
    ELSE 0 END)::bigint AS held
FROM billing.ledger_transfers t
WHERE t.merchant_id = sqlc.arg(merchant_id)::uuid AND t.credit_grant_id = sqlc.arg(credit_grant_id)::uuid AND t.book = 'general'
GROUP BY t.revenue_schedule_id, t.invoice_id, t.invoice_line_id
ORDER BY t.revenue_schedule_id NULLS LAST;

-- name: ListCreditReversalSchedules :many
-- What one reversal of a lot's payment took, per revenue schedule and its
-- invoice line (NULL: usage revenue's contra), that its recoveries have not
-- brought back: of its recognised part (contra) and of its deferred part.
SELECT t.revenue_schedule_id, t.invoice_id, t.invoice_line_id,
    COALESCE(sum(CASE WHEN t.source = 'money_recognized' AND t.transfer_type IN ('refund_recognized', 'chargeback_recognized') THEN t.amount
        WHEN t.source <> 'money_recognized' AND c.account_type IN ('refunds', 'chargebacks') THEN -t.amount ELSE 0 END), 0)::bigint AS contra,
    COALESCE(sum(CASE WHEN t.source = 'money_recognized' AND t.transfer_type IN ('refund_deferred', 'chargeback_deferred') THEN t.amount
        WHEN t.source <> 'money_recognized' AND c.account_type = 'deferred_revenue' THEN -t.amount ELSE 0 END), 0)::bigint AS deferred
FROM billing.ledger_transfers t
JOIN billing.ledger_accounts c ON c.merchant_id = t.merchant_id AND c.id = t.credit_account_id
WHERE t.merchant_id = sqlc.arg(merchant_id)::uuid AND t.credit_grant_id = sqlc.arg(credit_grant_id)::uuid AND t.book = 'general'
  AND (t.operation = 'purchased_credit_refund' AND t.source = 'money_recognized' AND t.source_id = sqlc.arg(reversal_id)::text
    OR t.source IN ('money_recognized_restore:' || sqlc.arg(reversal_id)::text, 'money_recognized_deferred_restore:' || sqlc.arg(reversal_id)::text))
GROUP BY t.revenue_schedule_id, t.invoice_id, t.invoice_line_id
ORDER BY t.revenue_schedule_id NULLS LAST;

-- name: ListScheduleLotMoney :many
-- What the lots' money funds now of each schedule of an invoice's lines, as
-- ListCreditLotRevenue counts it per lot, read through the lots that paid the
-- lines.
SELECT t.revenue_schedule_id::uuid AS revenue_schedule_id, sum(CASE
    WHEN t.transfer_type = 'credit_released' THEN t.amount
    WHEN t.operation = 'purchased_credit_refund' AND t.source = 'money_recognized' THEN -t.amount
    WHEN t.source LIKE 'money\_recognized\_%restore:%' THEN t.amount
    ELSE 0 END)::bigint AS held
FROM billing.ledger_transfers t
WHERE t.merchant_id = sqlc.arg(merchant_id)::uuid AND t.book = 'general' AND t.revenue_schedule_id = ANY(sqlc.arg(revenue_schedule_ids)::uuid[])
  AND t.credit_grant_id IN (
      SELECT l.credit_grant_id FROM billing.ledger_transfers l
      WHERE l.merchant_id = sqlc.arg(merchant_id)::uuid AND l.source = 'order_line' AND l.source_id = ANY(sqlc.arg(invoice_line_ids)::text[])
        AND l.credit_grant_id IS NOT NULL)
GROUP BY t.revenue_schedule_id
ORDER BY t.revenue_schedule_id;

-- name: ListUnscheduledCreditReleases :many
-- Lots' money released into a line priced in a merchant's own unit, net of
-- what reversals took back of it, under a schedule that does not exist:
-- revenue the books never recognise.
SELECT t.invoice_id::uuid AS invoice_id, t.invoice_line_id::uuid AS invoice_line_id, t.currency, sum(CASE
    WHEN t.transfer_type = 'credit_released' THEN t.amount
    WHEN t.operation = 'purchased_credit_refund' AND t.source = 'money_recognized' THEN -t.amount
    WHEN t.source LIKE 'money\_recognized\_%restore:%' THEN t.amount
    ELSE 0 END)::bigint AS amount
FROM billing.ledger_transfers t
WHERE t.merchant_id = sqlc.arg(merchant_id)::uuid AND t.book = 'general' AND t.credit_grant_id IS NOT NULL
  AND t.revenue_schedule_id IS NOT NULL AND t.invoice_id IS NOT NULL AND t.invoice_line_id IS NOT NULL
  AND NOT EXISTS (SELECT 1 FROM billing.revenue_schedules s WHERE s.merchant_id = t.merchant_id AND s.id = t.revenue_schedule_id)
GROUP BY t.invoice_id, t.invoice_line_id, t.currency
HAVING sum(CASE
    WHEN t.transfer_type = 'credit_released' THEN t.amount
    WHEN t.operation = 'purchased_credit_refund' AND t.source = 'money_recognized' THEN -t.amount
    WHEN t.source LIKE 'money\_recognized\_%restore:%' THEN t.amount
    ELSE 0 END) > 0
ORDER BY t.invoice_id, t.invoice_line_id, t.currency;

-- name: ListDueRevenueSchedules :many
-- CROSS-MERCHANT: the schedules due, in key order, by the due index alone: an
-- idle pass reads no row.
SELECT merchant_id, id, next_recognition_at::timestamptz AS next_recognition_at FROM billing.revenue_schedules
WHERE next_recognition_at <= sqlc.arg(now)::timestamptz
  AND (sqlc.narg(after_at)::timestamptz IS NULL
       OR (next_recognition_at, merchant_id, id)
          > (sqlc.narg(after_at)::timestamptz, sqlc.narg(after_merchant_id)::uuid, sqlc.narg(after_id)::uuid))
ORDER BY next_recognition_at, merchant_id, id
LIMIT sqlc.arg(row_limit)::int;

-- name: RevenueRecognitionDueBefore :one
-- CROSS-MERCHANT: whether a schedule has been due since before the instant.
SELECT EXISTS (
    SELECT 1 FROM billing.revenue_schedules
    WHERE next_recognition_at <= sqlc.arg(before)::timestamptz
)::boolean AS due;

-- name: ListInvoiceReceipts :many
-- The payments that brought an invoice money: positive charges that moved it at
-- a rail or were received outside OpenRails, the invoice's own payment first,
-- then by when each was taken.
SELECT p.* FROM billing.payments p
JOIN billing.invoices o ON o.merchant_id = p.merchant_id AND o.id = p.invoice_id
WHERE p.merchant_id = sqlc.arg(merchant_id)::uuid AND p.invoice_id = sqlc.arg(invoice_id)::uuid
  AND p.amount > 0 AND p.refunded_payment_id IS NULL AND p.reversal_kind IS NULL
  AND p.status IN ('succeeded', 'refunded') AND p.deleted_at IS NULL
  AND (p.money_movement = 'rail' OR p.channel = 'manual')
ORDER BY COALESCE(p.id = o.payment_id, false) DESC, p.purchased_at, p.id;

-- name: GetInvoiceNonCashSettlement :one
-- What the first settled provider bill of an invoice says was settled without
-- gateway money; 0 for an invoice no provider bill settled.
SELECT COALESCE((SELECT b.non_cash_amount FROM billing.provider_bills b
    WHERE b.merchant_id = sqlc.arg(merchant_id)::uuid AND b.invoice_id = sqlc.arg(invoice_id)::uuid AND b.settled_at IS NOT NULL
    ORDER BY b.settled_at, b.created_at LIMIT 1), 0)::bigint AS non_cash_amount;

-- name: GetPurchaseAccessByPayment :one
-- The access a payment bought of a product, once its grant exists.
SELECT * FROM billing.product_access
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND payment_id = sqlc.arg(payment_id)::uuid
  AND product_id = sqlc.arg(product_id)::uuid AND source_type = 'purchase' AND deleted_at IS NULL
ORDER BY starts_at, id
LIMIT 1;

-- name: ListOverappliedPayments :many
-- A payment whose cash the general ledger applied beyond what it received:
-- to its bill's lines at (application, payment, id), to the credit lot it
-- bought at (credit_issue, payment, id) and to the owed a usage invoice's
-- payment settles at (usage_collection*, payment, id). A won dispute's
-- recovery receives what it returns to unapplied cash.
SELECT source_id AS payment_id, currency,
       COALESCE(sum(amount) FILTER (WHERE transfer_type = 'cash_received' OR transfer_type = 'dispute_won' AND operation = 'dispute_won' AND revenue_schedule_id IS NULL), 0)::bigint AS received,
       COALESCE(sum(amount) FILTER (WHERE transfer_type IN ('cash_applied', 'usage_collected')), 0)::bigint AS applied
FROM billing.ledger_transfers
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND book = 'general' AND source = 'payment'
  AND transfer_type IN ('cash_received', 'cash_applied', 'usage_collected', 'dispute_won')
GROUP BY source_id, currency
HAVING COALESCE(sum(amount) FILTER (WHERE transfer_type IN ('cash_applied', 'usage_collected')), 0)
     > COALESCE(sum(amount) FILTER (WHERE transfer_type = 'cash_received' OR transfer_type = 'dispute_won' AND operation = 'dispute_won' AND revenue_schedule_id IS NULL), 0)
ORDER BY source_id, currency;

-- name: ListScheduledInvoiceLines :many
-- The lines among an invoice's that already have their revenue schedule.
SELECT invoice_line_id FROM billing.revenue_schedules
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND invoice_line_id = ANY(sqlc.arg(invoice_line_ids)::uuid[]);

-- name: ListInvoiceApplications :many
-- What the general ledger already applied of an invoice's money: each
-- payment's cash, at (application, payment, id), and the invoice's non-cash
-- settlement, at (application, invoice, id).
SELECT source, source_id, sum(amount)::bigint AS applied
FROM billing.ledger_transfers
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND book = 'general' AND operation = 'application'
  AND (source = 'payment' AND source_id = ANY(sqlc.arg(payment_ids)::text[]) OR source = 'order' AND source_id = sqlc.arg(invoice_id)::text)
GROUP BY source, source_id;

-- name: PaymentBookedCash :one
-- Whether the general ledger booked a payment's money.
SELECT EXISTS (
    SELECT 1 FROM billing.ledger_transfers
    WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND customer_id = sqlc.arg(customer_id)::uuid
      AND currency = sqlc.arg(currency)::text AND transfer_type = 'cash_received'
      AND operation = 'cash' AND source = 'payment' AND source_id = sqlc.arg(payment_id)::text
)::boolean AS booked;

-- name: ListUnscheduledInvoiceLines :many
-- A completed bill's line that sells revenue (not credit or usage) but has no
-- revenue schedule: a provider's tax and shipping line among them, which is
-- never scheduled. before_books is a line whose invoice was made and completed
-- before the merchant's books started.
WITH started AS (
    SELECT (SELECT started_at FROM billing.books_starts WHERE merchant_id = sqlc.arg(merchant_id)::uuid) AS at
)
SELECT l.invoice_id, l.id AS invoice_line_id, o.currency, l.amount,
       (l.source IS NOT DISTINCT FROM 'provider_adjustment')::boolean AS adjustment,
       COALESCE(o.created_at < s.at AND o.completed_at < s.at, true)::boolean AS before_books
FROM billing.invoice_lines l
JOIN billing.invoices o ON o.merchant_id = l.merchant_id AND o.id = l.invoice_id
CROSS JOIN started s
LEFT JOIN billing.products p ON p.merchant_id = l.merchant_id AND p.id = l.product_id
WHERE l.merchant_id = sqlc.arg(merchant_id)::uuid AND o.status = 'complete' AND o.origin <> 'usage' AND o.total > 0 AND l.amount > 0
  AND (l.credit_grant IS NULL OR l.credit_grant = 'null'::jsonb)
  AND (p.credit_grant IS NULL OR p.credit_grant = 'null'::jsonb)
  AND NOT EXISTS (SELECT 1 FROM billing.revenue_schedules r WHERE r.merchant_id = l.merchant_id AND r.invoice_line_id = l.id)
ORDER BY l.invoice_id, l.position;

-- name: SubscriptionHasLiveSchedules :one
-- Whether a subscription's revenue is still being recognised.
SELECT EXISTS (
    SELECT 1 FROM billing.revenue_schedules
    WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND subscription_id = sqlc.arg(subscription_id)::uuid
      AND next_recognition_at IS NOT NULL
)::boolean AS live;

-- name: ResolvePrunedBooksFindings :execrows
-- A prune undone: the books' findings it raised close.
UPDATE billing.reconciliation_findings
   SET status = 'fixed', resolution = 'auto_vanished', resolved_at = now()
 WHERE merchant_id = sqlc.arg(merchant_id)::uuid
   AND finding_type IN ('books.payment.pruned', 'books.subscription.pruned')
   AND evidence->>'run_id' = sqlc.arg(run_id)::text
   AND status IN ('reconcile_required', 'requires_review');
