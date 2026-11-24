-- name: LockRevenueSchedule :one
-- A schedule a reversal adjusts, locked against the recognition job.
SELECT * FROM billing.revenue_schedules
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid
FOR UPDATE;

-- name: AdjustRevenueSchedule :execrows
-- A reversal's change to a schedule: what it is funded with and has
-- recognised, as of when, and when it is next due.
UPDATE billing.revenue_schedules
SET funded = sqlc.arg(funded)::bigint,
    recognized_net = sqlc.arg(recognized_net)::bigint,
    recognized_through_at = sqlc.arg(recognized_through_at)::timestamptz,
    next_recognition_at = sqlc.narg(next_recognition_at)::timestamptz,
    reinstatements = sqlc.arg(reinstatements)::int,
    updated_at = now()
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid;

-- name: ListPaymentBooksLegs :many
-- The general-ledger legs a payment and its reversals posted, summed by
-- source, operation, type and schedule.
SELECT t.source_id, t.operation, t.transfer_type, t.revenue_schedule_id, sum(t.amount)::bigint AS amount
FROM billing.ledger_transfers t
WHERE t.merchant_id = sqlc.arg(merchant_id)::uuid AND t.book = 'general' AND t.source = 'payment'
  AND t.source_id IN (
      SELECT p.id::text FROM billing.payments p
      WHERE p.merchant_id = sqlc.arg(merchant_id)::uuid AND (p.id = sqlc.arg(payment_id)::uuid OR p.refunded_payment_id = sqlc.arg(payment_id)::uuid))
GROUP BY t.source_id, t.operation, t.transfer_type, t.revenue_schedule_id
ORDER BY t.source_id, t.operation, t.transfer_type, t.revenue_schedule_id;

-- name: PaymentPosted :one
-- Whether the general ledger holds a posting a payment row caused.
SELECT EXISTS (
    SELECT 1 FROM billing.ledger_transfers
    WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND book = 'general' AND source = 'payment' AND source_id = sqlc.arg(payment_id)::text
)::boolean AS posted;

-- name: ListUnpostedPayments :many
-- A receipt, or a settled reversal of one, that the general ledger holds no
-- posting of, stored no earlier than the payment it first booked (by the
-- clock that stamps both).
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
SELECT p.id, p.currency, p.amount, p.reversal_kind, p.created_at
FROM billing.payments p
CROSS JOIN started s
LEFT JOIN billing.payments o ON o.merchant_id = p.merchant_id AND o.id = p.refunded_payment_id
WHERE p.merchant_id = sqlc.arg(merchant_id)::uuid AND p.deleted_at IS NULL AND p.created_at >= s.at
  AND (p.refunded_payment_id IS NULL AND p.reversal_kind IS NULL AND p.amount > 0 AND p.status IN ('succeeded', 'refunded')
           AND (p.channel = 'manual' OR p.money_movement = 'rail' AND p.psp_id IS NOT NULL)
       OR p.refunded_payment_id IS NOT NULL AND p.status = 'succeeded' AND p.amount <> 0
           AND o.reversal_kind IS NULL AND o.amount > 0 AND o.status IN ('succeeded', 'refunded') AND o.deleted_at IS NULL
           AND (o.channel = 'manual' OR o.money_movement = 'rail' AND o.psp_id IS NOT NULL))
  AND NOT EXISTS (
      SELECT 1 FROM billing.ledger_transfers t
      WHERE t.merchant_id = p.merchant_id AND t.book = 'general' AND t.source = 'payment' AND t.source_id = p.id::text)
ORDER BY p.created_at, p.id;
