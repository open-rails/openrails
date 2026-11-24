-- name: CreateRevenueSchedule :one
-- A paid line's schedule, at its application, as its first recognition left
-- it. Once per line: a replay inserts nothing and returns no row.
INSERT INTO billing.revenue_schedules (
    merchant_id, id, customer_id, currency, order_id, order_line_id, subscription_id, product_access_id,
    product_id, price_id, method, revenue_account, funded, recognized_net,
    service_starts_at, service_ends_at, service_months, ended_at, recognized_through_at, next_recognition_at, reinstatements
) VALUES (
    sqlc.arg(merchant_id)::uuid, sqlc.arg(id)::uuid, sqlc.arg(customer_id)::uuid, sqlc.arg(currency)::text,
    sqlc.arg(order_id)::uuid, sqlc.arg(order_line_id)::uuid, sqlc.narg(subscription_id)::uuid, sqlc.narg(product_access_id)::uuid,
    sqlc.narg(product_id)::uuid, sqlc.narg(price_id)::uuid, sqlc.arg(method)::text, sqlc.arg(revenue_account)::text,
    sqlc.arg(funded)::bigint, sqlc.arg(recognized_net)::bigint,
    sqlc.arg(service_starts_at)::timestamptz, sqlc.arg(service_ends_at)::timestamptz, sqlc.narg(service_months)::int,
    sqlc.narg(ended_at)::timestamptz, sqlc.arg(recognized_through_at)::timestamptz, sqlc.narg(next_recognition_at)::timestamptz, 0
)
ON CONFLICT (merchant_id, order_line_id) DO NOTHING
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
    updated_at = now()
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid;

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

-- name: ListOrderReceipts :many
-- The payments that brought an order money: positive charges that moved it at
-- a rail or were received outside OpenRails, the order's own payment first,
-- then by when each was taken.
SELECT p.* FROM billing.payments p
JOIN billing.orders o ON o.merchant_id = p.merchant_id AND o.id = p.order_id
WHERE p.merchant_id = sqlc.arg(merchant_id)::uuid AND p.order_id = sqlc.arg(order_id)::uuid
  AND p.amount > 0 AND p.refunded_payment_id IS NULL AND p.reversal_kind IS NULL
  AND p.status IN ('succeeded', 'refunded') AND p.deleted_at IS NULL
  AND (p.money_movement = 'rail' OR p.channel = 'manual')
ORDER BY COALESCE(p.id = o.payment_id, false) DESC, p.purchased_at, p.id;

-- name: GetOrderNonCashSettlement :one
-- What the first settled provider bill of an order says was settled without
-- gateway money; 0 for an order no provider bill settled.
SELECT COALESCE((SELECT b.non_cash_amount FROM billing.provider_bills b
    WHERE b.merchant_id = sqlc.arg(merchant_id)::uuid AND b.order_id = sqlc.arg(order_id)::uuid AND b.settled_at IS NOT NULL
    ORDER BY b.settled_at, b.created_at LIMIT 1), 0)::bigint AS non_cash_amount;

-- name: GetPurchaseAccessByPayment :one
-- The access a payment bought of a product, once its grant exists.
SELECT * FROM billing.product_access
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND payment_id = sqlc.arg(payment_id)::uuid
  AND product_id = sqlc.arg(product_id)::uuid AND source_type = 'purchase' AND deleted_at IS NULL
ORDER BY starts_at, id
LIMIT 1;

-- name: ListOverappliedPayments :many
-- A payment whose cash the general ledger applied beyond what it received.
SELECT source_id AS payment_id, currency,
       COALESCE(sum(amount) FILTER (WHERE transfer_type = 'cash_received'), 0)::bigint AS received,
       COALESCE(sum(amount) FILTER (WHERE transfer_type = 'cash_applied'), 0)::bigint AS applied
FROM billing.ledger_transfers
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND book = 'general' AND source = 'payment'
  AND transfer_type IN ('cash_received', 'cash_applied')
GROUP BY source_id, currency
HAVING COALESCE(sum(amount) FILTER (WHERE transfer_type = 'cash_applied'), 0) > COALESCE(sum(amount) FILTER (WHERE transfer_type = 'cash_received'), 0)
ORDER BY source_id, currency;

-- name: ListScheduledOrderLines :many
-- The lines among an order's that already have their revenue schedule.
SELECT order_line_id FROM billing.revenue_schedules
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND order_line_id = ANY(sqlc.arg(order_line_ids)::uuid[]);

-- name: ListOrderApplications :many
-- What the general ledger already applied of an order's money: each
-- payment's cash, at (application, payment, id), and the order's non-cash
-- settlement, at (application, order, id).
SELECT source, source_id, sum(amount)::bigint AS applied
FROM billing.ledger_transfers
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND book = 'general' AND operation = 'application'
  AND (source = 'payment' AND source_id = ANY(sqlc.arg(payment_ids)::text[]) OR source = 'order' AND source_id = sqlc.arg(order_id)::text)
GROUP BY source, source_id;

-- name: PaymentBookedCash :one
-- Whether the general ledger booked a payment's money.
SELECT EXISTS (
    SELECT 1 FROM billing.ledger_transfers
    WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND customer_id = sqlc.arg(customer_id)::uuid
      AND currency = sqlc.arg(currency)::text AND transfer_type = 'cash_received'
      AND operation = 'cash' AND source = 'payment' AND source_id = sqlc.arg(payment_id)::text
)::boolean AS booked;

-- name: ListUnscheduledOrderLines :many
-- A completed bill's line that sells revenue (not credit or usage) but has no
-- revenue schedule: a provider's tax and shipping line among them, which is
-- never scheduled. before_books is a line whose order was made and completed
-- before the payment the merchant's general ledger first booked, by the
-- clock that stamps both.
WITH started AS (
    SELECT (
        SELECT p.created_at FROM billing.payments p
        WHERE p.merchant_id = sqlc.arg(merchant_id)::uuid AND p.id = (
            SELECT t.source_id::uuid FROM billing.ledger_transfers t
            WHERE t.merchant_id = sqlc.arg(merchant_id)::uuid AND t.book = 'general' AND t.operation = 'cash' AND t.source = 'payment'
            ORDER BY t.created_at, t.id
            LIMIT 1)
    ) AS at
)
SELECT l.order_id, l.id AS order_line_id, o.currency, l.amount,
       (l.source IS NOT DISTINCT FROM 'provider_adjustment')::boolean AS adjustment,
       COALESCE(o.created_at < s.at AND o.completed_at < s.at, true)::boolean AS before_books
FROM billing.order_lines l
JOIN billing.orders o ON o.merchant_id = l.merchant_id AND o.id = l.order_id
CROSS JOIN started s
LEFT JOIN billing.products p ON p.merchant_id = l.merchant_id AND p.id = l.product_id
WHERE l.merchant_id = sqlc.arg(merchant_id)::uuid AND o.status = 'complete' AND o.origin <> 'usage' AND o.total > 0 AND l.amount > 0
  AND (l.credit_grant IS NULL OR l.credit_grant = 'null'::jsonb)
  AND (p.credit_grant IS NULL OR p.credit_grant = 'null'::jsonb)
  AND NOT EXISTS (SELECT 1 FROM billing.revenue_schedules r WHERE r.merchant_id = l.merchant_id AND r.order_line_id = l.id)
ORDER BY l.order_id, l.position;

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
