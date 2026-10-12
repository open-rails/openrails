-- Usage orders (orders.origin = 'usage'): owed charges billed in arrears for
-- a period. Pending charges wait in owed_items until an order bills them.

-- name: ListUsagePayers :many
-- Every (payer, currency) with ledger money movement since active_since: usage
-- is posted when it is recorded, so a payer with no movement has nothing to
-- bill and the sweep scales with activity, not with every payer on file.
-- period_anchor is the payer's first recorded activity (GetCustomerPeriodAnchor):
-- its first transfer or the opening of its balance account. Both are
-- permanent, so anniversary windows never move.
WITH active AS (
    SELECT DISTINCT lt.customer_id, lt.currency
    FROM billing.ledger_transfers lt
    WHERE lt.merchant_id = sqlc.arg(merchant_id)::uuid AND lt.customer_id IS NOT NULL
      AND lt.created_at >= sqlc.arg(active_since)::timestamptz
)
SELECT p.customer_id::uuid AS customer_id, p.currency,
       LEAST(
           (SELECT MIN(lt.created_at) FROM billing.ledger_transfers lt
            WHERE lt.merchant_id = sqlc.arg(merchant_id)::uuid AND lt.customer_id = p.customer_id AND lt.currency = p.currency),
           (SELECT a.created_at FROM billing.ledger_accounts a
            WHERE a.merchant_id = sqlc.arg(merchant_id)::uuid AND a.customer_id = p.customer_id
              AND a.currency = p.currency AND a.account_type = 'customer_balance'))::timestamptz AS period_anchor
FROM active p
ORDER BY p.customer_id, p.currency;

-- name: GetUsageOrderByPeriod :one
-- One usage order per (payer, currency, period).
SELECT * FROM billing.orders
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND customer_id = sqlc.arg(customer_id)::uuid AND origin = 'usage'
  AND currency = sqlc.arg(currency)::text
  AND period_starts_at = sqlc.arg(period_starts_at)::timestamptz AND period_ends_at = sqlc.arg(period_ends_at)::timestamptz;

-- name: CreateUsageOrder :exec
-- An open usage order; one a funded balance already paid in full is created
-- complete, numbered.
INSERT INTO billing.orders (
    merchant_id, id, customer_id, origin, status, payment_status, currency, total, amount_paid, number,
    period_starts_at, period_ends_at, due_at, completed_at, collection_method, po_number, memo, tax, billing_contacts,
    collection_failure_count, created_at, updated_at
) VALUES (
    sqlc.arg(merchant_id)::uuid, sqlc.arg(id)::uuid, sqlc.arg(customer_id)::uuid, 'usage',
    CASE WHEN sqlc.narg(number)::text IS NULL THEN 'open' ELSE 'complete' END,
    CASE WHEN sqlc.narg(number)::text IS NULL THEN 'requires_payment_method' ELSE 'succeeded' END,
    sqlc.arg(currency)::text, sqlc.arg(total)::bigint, sqlc.arg(amount_paid)::bigint, sqlc.narg(number)::text,
    sqlc.arg(period_starts_at)::timestamptz, sqlc.arg(period_ends_at)::timestamptz, sqlc.arg(due_at)::timestamptz,
    CASE WHEN sqlc.narg(number)::text IS NULL THEN NULL ELSE sqlc.arg(now)::timestamptz END,
    sqlc.arg(collection_method)::text, sqlc.narg(po_number)::text, sqlc.narg(memo)::text, sqlc.arg(tax)::jsonb, sqlc.arg(billing_contacts)::jsonb,
    0, sqlc.arg(now)::timestamptz, sqlc.arg(now)::timestamptz
);

-- name: CreateUsageOrderLine :exec
INSERT INTO billing.order_lines (merchant_id, id, order_id, customer_id, "position", description, unit_amount, amount, source, created_at)
VALUES (sqlc.arg(merchant_id)::uuid, sqlc.arg(id)::uuid, sqlc.arg(order_id)::uuid, sqlc.arg(customer_id)::uuid, sqlc.arg(position)::int,
        sqlc.arg(source)::text, sqlc.arg(amount)::bigint, sqlc.arg(amount)::bigint, sqlc.arg(source)::text, sqlc.arg(now)::timestamptz);

-- name: InsertPendingOwedItem :exec
-- owed_items is the pending-accrual workspace: rows are born pending and only
-- leave via AttachPendingOwedItemsToOrder.
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

-- name: AttachPendingOwedItemsToOrder :execrows
UPDATE billing.owed_items
SET order_id = sqlc.arg(order_id)::uuid,
    status = 'billed',
    updated_at = sqlc.arg(now)::timestamptz
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND customer_id = sqlc.arg(customer_id)::uuid
  AND currency = sqlc.arg(currency)
  AND order_id IS NULL
  AND status = 'pending'
  AND accrued_at >= sqlc.arg(period_starts_at)::timestamptz
  AND accrued_at < sqlc.arg(period_ends_at)::timestamptz;

-- name: SumPendingOwedItemAmountBySourceInPeriod :many
-- A period's pending charges per accrual source: a usage order's lines.
-- Metered accruals carry their rating identity (e.g. metered:<meter>) in
-- metadata->>'source'.
SELECT COALESCE(NULLIF(metadata ->> 'source', ''), source_type)::text AS source,
       COALESCE(SUM(amount), 0)::bigint AS amount,
       COUNT(*)::bigint AS item_count
FROM billing.owed_items
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND customer_id = sqlc.arg(customer_id)::uuid AND currency = sqlc.arg(currency)
  AND order_id IS NULL AND status = 'pending'
  AND accrued_at >= sqlc.arg(period_starts_at)::timestamptz
  AND accrued_at < sqlc.arg(period_ends_at)::timestamptz
GROUP BY 1
ORDER BY 1 ASC;

-- name: ListUsageThresholdCandidates :many
-- Arrears payers whose pending charges plus open usage orders reached their
-- trigger: the payer's billing policy's collection_threshold_amount when it
-- has one, else the merchant-wide threshold, else the payer's own credit
-- line. policies is the merchant's settings, {policies: {name: body}, tiers:
-- {tier: name}, default: name}; the payer's policy is its own assignment while
-- the settings still declare it, else its tier's (money_settings.tier), else
-- the default.
SELECT s.customer_id, s.currency, MIN(oi.accrued_at)::timestamptz AS period_starts_at, MIN(s.created_at)::timestamptz AS period_anchor
FROM billing.money_settings s
JOIN billing.owed_items oi
  ON oi.merchant_id = s.merchant_id
 AND oi.customer_id = s.customer_id
 AND oi.currency = s.currency
 AND oi.order_id IS NULL
 AND oi.status = 'pending'
 AND oi.accrued_at < sqlc.arg(cutoff)::timestamptz
JOIN billing.customers c ON c.merchant_id = s.merchant_id AND c.id = s.customer_id
CROSS JOIN LATERAL (
    SELECT (sqlc.arg(policies)::jsonb -> 'policies' -> COALESCE(
        CASE WHEN sqlc.arg(policies)::jsonb -> 'policies' -> c.billing_policy IS NOT NULL THEN c.billing_policy END,
        sqlc.arg(policies)::jsonb -> 'tiers' ->> s.tier,
        sqlc.arg(policies)::jsonb ->> 'default') ->> 'collection_threshold_amount')::bigint AS threshold
) pol
WHERE s.merchant_id = sqlc.arg(merchant_id)::uuid
  AND s.billing_mode = 'arrears'
  AND s.credit_limit_amount > 0
GROUP BY s.merchant_id, s.customer_id, s.currency, s.credit_limit_amount, pol.threshold
HAVING COALESCE(SUM(oi.amount), 0)::bigint + (
    SELECT COALESCE(SUM(o.total - o.amount_paid), 0)::bigint
    FROM billing.orders o
    WHERE o.merchant_id = s.merchant_id AND o.customer_id = s.customer_id AND o.currency = s.currency
      AND o.origin = 'usage' AND o.status IN ('open', 'processing')
) >= COALESCE(
    pol.threshold,
    CASE WHEN sqlc.arg(min_threshold)::bigint > 0 THEN sqlc.arg(min_threshold)::bigint ELSE s.credit_limit_amount END)
ORDER BY period_starts_at ASC;

-- name: ListChargeableUsageOrders :many
-- Open usage orders the scheduled collection charges now: charge_automatically,
-- due, on their first try or a retry that came due.
SELECT o.id, o.customer_id, o.total - o.amount_paid AS amount_due
FROM billing.orders o
JOIN billing.money_settings s
  ON s.merchant_id = o.merchant_id AND s.customer_id = o.customer_id AND s.currency = o.currency
WHERE o.merchant_id = sqlc.arg(merchant_id)::uuid
  AND o.origin = 'usage' AND o.status = 'open'
  AND o.collection_method = 'charge_automatically'
  AND o.amount_paid < o.total
  AND s.default_payment_method_id IS NOT NULL
  AND o.due_at <= sqlc.arg(now)::timestamptz
  AND ((o.collection_failure_count = 0 AND o.next_collection_attempt_at IS NULL)
       OR o.next_collection_attempt_at <= sqlc.arg(now)::timestamptz)
  AND (sqlc.arg(min_threshold)::bigint <= 0 OR o.total - o.amount_paid >= sqlc.arg(min_threshold)::bigint)
ORDER BY o.due_at, o.created_at;

-- name: LockUsageOrder :one
SELECT * FROM billing.orders
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND customer_id = sqlc.arg(customer_id)::uuid AND id = sqlc.arg(id)::uuid AND origin = 'usage'
FOR UPDATE;

-- name: StartUsageCollection :execrows
-- Points the usage order at its one live collection, its attempt. Collecting
-- an uncollectible order reopens it.
UPDATE billing.orders
SET status = 'processing', payment_status = 'processing', attempt_id = sqlc.arg(attempt_id)::uuid,
    payment_method_id = sqlc.arg(payment_method_id)::uuid, psp_id = sqlc.arg(psp_id)::uuid,
    next_collection_attempt_at = NULL, uncollectible_at = NULL, updated_at = sqlc.arg(now)::timestamptz
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid AND origin = 'usage'
  AND status IN ('open', 'uncollectible') AND amount_paid < total;

-- name: EndUsageCollection :execrows
-- A collection that moved no money, settled, or was not executed: the order
-- takes payment again, its next scheduled try at next_attempt_at (NULL: none
-- scheduled).
UPDATE billing.orders
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
UPDATE billing.orders
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
UPDATE billing.orders
SET collection_failure_count = collection_failure_count + 1,
    collection_failed_at = COALESCE(collection_failed_at, sqlc.arg(now)::timestamptz),
    next_collection_attempt_at = NULL,
    last_payment_error = sqlc.arg(last_payment_error)::jsonb,
    updated_at = sqlc.arg(now)::timestamptz
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid AND origin = 'usage' AND status = 'open';

-- name: ResumeStoppedUsageCollection :execrows
-- A payer designating a collection card did what a stopped collection asked:
-- its usage orders are due again at once. Uncollectible ones need the
-- merchant, and live collections belong to their verifier.
UPDATE billing.orders
SET next_collection_attempt_at = sqlc.arg(now)::timestamptz,
    updated_at = sqlc.arg(now)::timestamptz
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND customer_id = sqlc.arg(customer_id)::uuid AND currency = sqlc.arg(currency)
  AND origin = 'usage' AND status = 'open' AND amount_paid < total
  AND collection_method = 'charge_automatically'
  AND collection_failure_count > 0 AND next_collection_attempt_at IS NULL;

-- name: EarliestPendingOwedAccrual :one
-- When the customer's unbilled usage in the currency began; none for none.
SELECT accrued_at FROM billing.owed_items
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND customer_id = sqlc.arg(customer_id)::uuid AND currency = sqlc.arg(currency)::text
  AND order_id IS NULL AND status = 'pending'
ORDER BY accrued_at
LIMIT 1;

-- name: ListOpenUsageOrdersOfCurrency :many
-- The customer's open usage orders in the currency, oldest first: at most a
-- few, each a closed billing window.
SELECT * FROM billing.orders
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND customer_id = sqlc.arg(customer_id)::uuid AND currency = sqlc.arg(currency)::text
  AND origin = 'usage' AND status = 'open' AND amount_paid < total
ORDER BY created_at, id
LIMIT sqlc.arg(row_limit)::int;

-- name: NotifyOverdueUsageOrders :one
-- Tells each payer once about each open usage order past its due date; the
-- notice id is the order's, so a later pass adds none. Its seed is the one
-- earlier releases gave an invoice's notice, which kept the order's id.
WITH notices AS (
    INSERT INTO billing.notification_events (id, merchant_id, customer_id, event_type, data, created_at)
    SELECT md5('invoice_overdue:' || id::text)::uuid, merchant_id, customer_id, 'order_overdue',
           jsonb_build_object('order_id', 'ord_' || id::text, 'amount_due', (total - amount_paid)::text, 'currency', currency, 'due_at', due_at),
           sqlc.arg(now)::timestamptz
    FROM billing.orders
    WHERE merchant_id = sqlc.arg(merchant_id)::uuid
      AND origin = 'usage' AND status IN ('open', 'processing') AND amount_paid < total
      AND due_at < sqlc.arg(now)::timestamptz
    ON CONFLICT (merchant_id, id) DO NOTHING
    RETURNING id
)
SELECT count(*) FROM notices;

-- name: PayUsageOrder :one
-- Applies a payment, remittance or balance repayment to a usage order. A live
-- collection keeps its claim: only its own settlement pays the order then.
UPDATE billing.orders
SET amount_paid = amount_paid + sqlc.arg(amount)::bigint,
    next_collection_attempt_at = CASE WHEN amount_paid + sqlc.arg(amount)::bigint = total THEN NULL ELSE next_collection_attempt_at END,
    last_payment_error = CASE WHEN amount_paid + sqlc.arg(amount)::bigint = total THEN NULL ELSE last_payment_error END,
    updated_at = sqlc.arg(now)::timestamptz
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid AND origin = 'usage'
  AND (status IN ('open', 'uncollectible') OR (status = 'processing' AND attempt_id = sqlc.narg(attempt_id)::uuid))
  AND amount_paid + sqlc.arg(amount)::bigint <= total
RETURNING *;

-- name: CompleteUsageOrder :execrows
-- A usage order paid in full: numbered and complete. payment_id is the
-- payment that completed it, NULL when a funded balance did.
UPDATE billing.orders
SET status = 'complete', payment_status = 'succeeded', number = sqlc.arg(number)::text,
    payment_id = COALESCE(sqlc.narg(payment_id)::uuid, payment_id), completed_at = sqlc.arg(now)::timestamptz,
    uncollectible_at = NULL, updated_at = sqlc.arg(now)::timestamptz
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid AND origin = 'usage'
  AND status IN ('open', 'processing', 'uncollectible') AND amount_paid = total;

-- name: VoidUsageOrder :execrows
UPDATE billing.orders
SET status = 'void', payment_status = 'requires_payment_method', voided_at = sqlc.arg(now)::timestamptz,
    next_collection_attempt_at = NULL, uncollectible_at = NULL, updated_at = sqlc.arg(now)::timestamptz
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid AND origin = 'usage'
  AND status IN ('open', 'uncollectible');

-- name: MarkUsageOrderUncollectible :execrows
UPDATE billing.orders
SET status = 'uncollectible', uncollectible_at = sqlc.arg(now)::timestamptz, next_collection_attempt_at = NULL,
    updated_at = sqlc.arg(now)::timestamptz
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid AND origin = 'usage'
  AND status = 'open' AND amount_paid < total;

-- name: ListOwedOrderClaims :many
-- Usage orders that still claim a customer's owed money, oldest first, locked
-- so a collection cannot start on one while funding repays it. A processing
-- one has a collection in flight.
SELECT id, total - amount_paid AS amount_due, status
FROM billing.orders
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND customer_id = sqlc.arg(customer_id)::uuid AND currency = sqlc.arg(currency)::text
  AND origin = 'usage' AND status IN ('open', 'processing', 'uncollectible') AND amount_paid < total
ORDER BY due_at, created_at, id
FOR UPDATE;

-- name: SumOwedOrderClaims :one
SELECT COALESCE(SUM(total - amount_paid), 0)::bigint AS amount
FROM billing.orders
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND customer_id = sqlc.arg(customer_id)::uuid AND currency = sqlc.arg(currency)::text
  AND origin = 'usage' AND status IN ('open', 'processing', 'uncollectible') AND amount_paid < total;

-- name: CountOrderAttempts :one
-- An order's attempts so far: a scheduled collection's ordinal.
SELECT count(*)::bigint FROM billing.order_attempts
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND order_id = sqlc.arg(order_id)::uuid;

-- name: InsertOrderPayment :exec
-- A payment that paid a usage order: a settled collection charge or a
-- recorded remittance, with the owed-payment transfer that settled it.
INSERT INTO billing.payments (
    id, merchant_id, customer_id, order_id, ledger_transfer_id, channel, rail, psp_id, transaction_id,
    amount, list_amount, currency, status, purchased_at, created_at, money_movement, token_type
) VALUES (
    sqlc.arg(id)::uuid, sqlc.arg(merchant_id)::uuid, sqlc.arg(customer_id)::uuid, sqlc.arg(order_id)::uuid, sqlc.arg(ledger_transfer_id)::uuid,
    sqlc.arg(channel)::text, sqlc.narg(rail)::text, sqlc.narg(psp_id)::uuid, sqlc.arg(transaction_id)::text,
    sqlc.arg(amount)::bigint, sqlc.arg(amount)::bigint, sqlc.arg(currency)::text, 'succeeded', sqlc.arg(paid_at)::timestamptz, sqlc.arg(now)::timestamptz,
    CASE WHEN sqlc.arg(channel)::text = 'rail' THEN 'rail' ELSE 'none' END, sqlc.narg(token_type)::text
);

-- name: GetOrderPayment :one
SELECT * FROM billing.payments
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND customer_id = sqlc.arg(customer_id)::uuid
  AND order_id = sqlc.arg(order_id)::uuid AND id = sqlc.arg(id)::uuid;

-- name: ListOrderCollectionsForArchive :many
-- Every terminal order collection with the payment its payload names, if
-- any, and that payment's ledger transfer: the archive checks each pair
-- before exporting or restoring it.
SELECT sqlc.embed(i), a.id AS payment_id, a.customer_id AS payment_customer_id, a.order_id AS payment_order_id,
    a.psp_id AS payment_psp_id, a.rail AS payment_rail, a.transaction_id AS payment_transaction_id, a.amount AS payment_amount,
    a.currency AS payment_currency, a.status AS payment_status, l.amount AS ledger_amount,
    COALESCE(l.merchant_id = a.merchant_id AND l.customer_id = a.customer_id
        AND l.order_id = a.order_id AND l.currency = a.currency
        AND l.source = 'invoice_charge' AND l.source_id = i.idempotency_key
        AND l.operation = 'invoice_payment' AND l.transfer_type = 'owed_payment', false)::boolean AS ledger_matches
FROM billing.provider_intents i
LEFT JOIN billing.payments a ON a.merchant_id = i.merchant_id AND a.id = (i.payload->>'payment_id')::uuid
LEFT JOIN billing.ledger_transfers l ON l.merchant_id = a.merchant_id AND l.id = a.ledger_transfer_id
WHERE i.merchant_id = sqlc.arg(merchant_id)::uuid AND i.intent_type = 'order_collection'
  AND (sqlc.narg(after_id)::uuid IS NULL OR i.id > sqlc.narg(after_id)::uuid)
ORDER BY i.id
LIMIT sqlc.arg(page_size)::int;

-- name: GetUsageMonthlyCollectionPeriod :one
SELECT monthly_period_started_at FROM billing.usage_collection_cadence
WHERE merchant_id = $1;

-- name: CompleteUsageMonthlyCollectionPeriod :exec
INSERT INTO billing.usage_collection_cadence (merchant_id, monthly_period_started_at, completed_at)
VALUES ($1, $2, $3)
ON CONFLICT (merchant_id) DO UPDATE
SET monthly_period_started_at = EXCLUDED.monthly_period_started_at,
    completed_at = GREATEST(billing.usage_collection_cadence.completed_at, EXCLUDED.completed_at)
WHERE EXCLUDED.monthly_period_started_at >= billing.usage_collection_cadence.monthly_period_started_at;

-- name: TryLockUsageMonthlyCollection :one
SELECT pg_try_advisory_lock(hashtextextended('openrails.usage_monthly_collection:' || sqlc.arg(merchant_id)::uuid::text, 0))::boolean;

-- name: UnlockUsageMonthlyCollection :exec
SELECT pg_advisory_unlock(hashtextextended('openrails.usage_monthly_collection:' || sqlc.arg(merchant_id)::uuid::text, 0));

-- name: GetUsageRecoveryPayment :many
SELECT p.* FROM billing.payments p
JOIN billing.orders o ON o.merchant_id = p.merchant_id AND o.id = p.order_id AND o.origin = 'usage'
WHERE p.merchant_id = sqlc.arg(merchant_id)::uuid AND p.psp_id = sqlc.arg(psp_id)::uuid
  AND p.transaction_id = sqlc.arg(transaction_id)::text
LIMIT 2;

-- name: UsageRecoveryHasOtherPayment :one
-- A transaction already allocated to a subscription or purchase cannot also
-- settle a usage order, including soft-deleted financial rows.
SELECT EXISTS(SELECT 1 FROM billing.payments p
    WHERE p.merchant_id = sqlc.arg(merchant_id)::uuid AND p.psp_id = sqlc.arg(psp_id)::uuid AND p.transaction_id = sqlc.arg(transaction_id)::text
      AND NOT EXISTS (SELECT 1 FROM billing.orders o WHERE o.merchant_id = p.merchant_id AND o.id = p.order_id AND o.origin = 'usage'))::boolean;

-- name: UsageRecoveryVaultCustomers :many
SELECT DISTINCT customer_id FROM billing.payment_methods
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND psp_id = sqlc.arg(psp_id)::uuid
  AND rail = 'nmi' AND rail_customer_ref = sqlc.arg(vault)::text
LIMIT 2;

-- name: UsageRecoveryAllocationTotal :one
-- The order's retained paid amount must be explained by its allocations,
-- including native rounding (provider amount can exceed the allocated debt).
SELECT COALESCE(sum(l.amount), 0)::bigint AS amount,
  COALESCE(bool_and(l.id IS NOT NULL AND l.order_id IS NOT DISTINCT FROM p.order_id AND l.customer_id IS NOT DISTINCT FROM p.customer_id
    AND l.currency = p.currency AND l.currency = sqlc.arg(currency)::text
    AND l.transfer_type = 'owed_payment' AND l.operation IN ('invoice_payment', 'manual_invoice_payment')
    AND l.amount > 0 AND p.amount >= l.amount), true)::boolean AS consistent
FROM billing.payments p LEFT JOIN billing.ledger_transfers l
  ON l.merchant_id = p.merchant_id AND l.id = p.ledger_transfer_id
WHERE p.merchant_id = sqlc.arg(merchant_id)::uuid AND p.order_id = sqlc.arg(order_id)::uuid AND p.status = 'succeeded' AND p.deleted_at IS NULL;

-- name: GetUsageRecoveryLedgerTransfer :one
SELECT * FROM billing.ledger_transfers
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(transfer_id)::uuid;

-- name: UsageRecoveryAccounts :many
-- A different chosen card does not hide a prior account's paid order, even
-- if that card was added after the backup. Inspect every known card account
-- in this deployment environment; unrelated rails cannot collect it.
SELECT p.id, p.rail FROM billing.psps p
WHERE p.merchant_id = sqlc.arg(merchant_id)::uuid AND p.rail IN ('nmi', 'stripe')
  AND p.environment = (SELECT environment FROM billing.psps WHERE merchant_id = p.merchant_id AND id = sqlc.arg(routed_psp)::uuid)
ORDER BY p.id;
