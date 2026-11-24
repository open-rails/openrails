-- name: LockUsageReceivable :one
-- What the general ledger holds receivable from a customer in a currency,
-- its account locked so concurrent collections, waivers and allowances cap
-- at the same balance.
SELECT (debits_posted - credits_posted)::bigint AS receivable
FROM billing.ledger_accounts
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND book = 'general' AND account_type = 'usage_receivable'
  AND currency = sqlc.arg(currency)::text AND customer_id = sqlc.arg(customer_id)::uuid
FOR UPDATE;

-- name: GetUsageAllowance :one
-- A usage order's allowance for doubtful accounts, and how many legs moved
-- it, at (allowance, order:<id>, n): bad_debt raises it; bad_debt_recovery
-- and a write-off (usage_waived) lower it.
SELECT (COALESCE(sum(amount) FILTER (WHERE transfer_type = 'bad_debt'), 0)
      - COALESCE(sum(amount) FILTER (WHERE transfer_type <> 'bad_debt'), 0))::bigint AS allowance,
       count(*)::bigint AS legs
FROM billing.ledger_transfers
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND book = 'general' AND source = sqlc.arg(source)::text AND operation = 'allowance';

-- name: GetCustomerUsageAllowance :one
-- A customer's allowance for doubtful accounts in a currency, every usage
-- order's together.
SELECT (COALESCE(sum(amount) FILTER (WHERE transfer_type = 'bad_debt'), 0)
      - COALESCE(sum(amount) FILTER (WHERE transfer_type <> 'bad_debt'), 0))::bigint AS allowance
FROM billing.ledger_transfers
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND customer_id = sqlc.arg(customer_id)::uuid AND currency = sqlc.arg(currency)::text
  AND book = 'general' AND operation = 'allowance';

-- name: UsageCollectionPosted :one
-- Whether the general ledger applied a usage order payment's cash.
SELECT EXISTS (
    SELECT 1 FROM billing.ledger_transfers
    WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND book = 'general' AND source = 'payment' AND source_id = sqlc.arg(payment_id)::text
      AND operation IN ('usage_collection', 'usage_collection_revenue')
)::boolean AS posted;

-- name: PaymentReversalPosted :one
-- Whether the general ledger holds a posting of a payment's reversal.
SELECT EXISTS (
    SELECT 1 FROM billing.ledger_transfers
    WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND book = 'general' AND source = 'payment' AND source_id = sqlc.arg(reversal_id)::text
)::boolean AS posted;

-- name: ListUsagePaymentReversals :many
-- A usage order payment's chargebacks and won disputes, settled, in the
-- order they were stored.
SELECT * FROM billing.payments
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND refunded_payment_id = sqlc.arg(payment_id)::uuid
  AND reversal_kind IN ('chargeback', 'dispute_reversal') AND status = 'succeeded' AND deleted_at IS NULL
ORDER BY created_at, id;

-- name: GetUsagePaymentReopened :one
-- What the units book reopened of a usage order payment's owed on its
-- chargebacks, and paid again on their won disputes, at (_, usage_payment:<id>, _).
SELECT COALESCE(sum(amount) FILTER (WHERE transfer_type = 'owed_reopened'), 0)::bigint AS reopened,
       COALESCE(sum(amount) FILTER (WHERE transfer_type = 'owed_payment'), 0)::bigint AS resettled
FROM billing.ledger_transfers
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND book = 'units' AND source = sqlc.arg(source)::text;

-- name: ListUsageReceivableDrift :many
-- Each customer and currency whose units owed (customer_owed) differs from
-- the general-ledger usage receivable: owed that accrued before the books
-- has none.
WITH owed AS (
    SELECT customer_id, currency, (debits_posted - credits_posted)::bigint AS amount
    FROM billing.ledger_accounts
    WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND book = 'units' AND account_type = 'customer_owed'
), receivable AS (
    SELECT customer_id, currency, (debits_posted - credits_posted)::bigint AS amount
    FROM billing.ledger_accounts
    WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND book = 'general' AND account_type = 'usage_receivable'
)
SELECT COALESCE(o.customer_id, r.customer_id)::uuid AS customer_id, COALESCE(o.currency, r.currency)::text AS currency,
       COALESCE(o.amount, 0)::bigint AS owed, COALESCE(r.amount, 0)::bigint AS receivable
FROM owed o
FULL JOIN receivable r ON r.customer_id = o.customer_id AND r.currency = o.currency
WHERE COALESCE(o.amount, 0) <> COALESCE(r.amount, 0)
ORDER BY 1, 2;
