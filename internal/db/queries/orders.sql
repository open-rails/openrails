-- Orders: purchases, their lines, ownership claims and numbers.

-- name: CreateOrder :exec
INSERT INTO billing.orders (
    merchant_id, id, customer_id, origin, status, payment_status, currency, total,
    idempotency_key, request_digest, lines_digest, expires_at, created_at, updated_at
) VALUES (
    sqlc.arg(merchant_id)::uuid, sqlc.arg(id)::uuid, sqlc.arg(customer_id)::uuid, sqlc.arg(origin)::text,
    'open', 'requires_payment_method', sqlc.arg(currency)::text, sqlc.arg(total)::bigint,
    sqlc.narg(idempotency_key)::text, sqlc.narg(request_digest)::bytea, sqlc.narg(lines_digest)::bytea,
    sqlc.arg(expires_at)::timestamptz, sqlc.arg(now)::timestamptz, sqlc.arg(now)::timestamptz
);

-- name: GetOpenOrderByLines :one
-- The customer's open order of these lines, expired or not.
SELECT id, expires_at FROM billing.orders
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND customer_id = sqlc.arg(customer_id)::uuid
  AND lines_digest = sqlc.arg(lines_digest)::bytea AND status = 'open';

-- name: CreateRenewalOrder :execrows
-- A renewal's order, once per subscription and period: zero rows when it
-- exists.
INSERT INTO billing.orders (
    merchant_id, id, customer_id, origin, status, payment_status, currency, total,
    subscription_id, period_starts_at, period_ends_at, due_at, created_at, updated_at, subscription_bill_kind
) VALUES (
    sqlc.arg(merchant_id)::uuid, sqlc.arg(id)::uuid, sqlc.arg(customer_id)::uuid, 'subscription',
    'open', 'requires_payment_method', sqlc.arg(currency)::text, sqlc.arg(total)::bigint,
    sqlc.arg(subscription_id)::uuid, sqlc.arg(period_starts_at)::timestamptz, sqlc.arg(period_ends_at)::timestamptz,
    sqlc.arg(due_at)::timestamptz, sqlc.arg(now)::timestamptz, sqlc.arg(now)::timestamptz, 'renewal'
)
ON CONFLICT (merchant_id, subscription_id, period_starts_at) WHERE origin = 'subscription' AND collection_owner = 'engine' AND subscription_bill_kind = 'renewal' DO NOTHING;

-- name: GetRenewalOrder :one
SELECT * FROM billing.orders
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND origin = 'subscription' AND collection_owner = 'engine' AND subscription_bill_kind = 'renewal'
  AND subscription_id = sqlc.arg(subscription_id)::uuid AND period_starts_at = sqlc.arg(period_starts_at)::timestamptz;

-- name: VoidRenewalOrder :execrows
-- A live renewal its subscription's end left unowed. A payment that landed
-- anyway stays on it (refunded).
UPDATE billing.orders
SET status = 'void', voided_at = sqlc.arg(now)::timestamptz, updated_at = sqlc.arg(now)::timestamptz,
    payment_id = sqlc.narg(payment_id)::uuid,
    payment_status = CASE WHEN sqlc.narg(payment_id)::uuid IS NULL THEN 'requires_payment_method' ELSE 'succeeded' END
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid
  AND origin = 'subscription' AND status IN ('open', 'processing');

-- name: GetOpenRenewalOrder :one
-- The subscription's unpaid renewal, if any.
SELECT * FROM billing.orders
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND origin = 'subscription' AND collection_owner = 'engine' AND subscription_bill_kind = 'renewal'
  AND subscription_id = sqlc.arg(subscription_id)::uuid AND status IN ('open', 'processing')
ORDER BY period_starts_at DESC
LIMIT 1;

-- name: CreateOrderLine :exec
INSERT INTO billing.order_lines (
    merchant_id, id, order_id, customer_id, "position", price_id, product_id, description,
    quantity, unit_amount, amount, ownership, claim_key, billing_interval_hours, billing_interval_months,
    access_duration_hours, provider_access_default, credit_grant, created_at
) VALUES (
    sqlc.arg(merchant_id)::uuid, sqlc.arg(id)::uuid, sqlc.arg(order_id)::uuid, sqlc.arg(customer_id)::uuid,
    sqlc.arg(position)::int, sqlc.arg(price_id)::uuid, sqlc.arg(product_id)::uuid, sqlc.arg(description)::text,
    sqlc.narg(quantity)::int, sqlc.arg(unit_amount)::bigint, sqlc.arg(amount)::bigint, sqlc.arg(ownership)::text,
    sqlc.narg(claim_key)::text, sqlc.narg(billing_interval_hours)::int, sqlc.narg(billing_interval_months)::int, sqlc.narg(access_duration_hours)::int, sqlc.arg(provider_access_default)::boolean,
    sqlc.narg(credit_grant)::jsonb, sqlc.arg(now)::timestamptz
);

-- name: GetOrder :one
SELECT * FROM billing.orders
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid;

-- name: GetCustomerOrder :one
SELECT * FROM billing.orders
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND customer_id = sqlc.arg(customer_id)::uuid AND id = sqlc.arg(id)::uuid;

-- name: LockOrder :one
SELECT * FROM billing.orders
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid
FOR UPDATE;

-- name: GetOrderByIdempotencyKey :one
SELECT * FROM billing.orders
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND customer_id = sqlc.arg(customer_id)::uuid
  AND idempotency_key = sqlc.arg(idempotency_key)::text;

-- name: ListOrdersPage :many
-- One page of orders, newest first; every filter is optional. price_id names
-- orders with a line on that price; overdue_at the open ones due before it.
SELECT o.* FROM billing.orders o
WHERE o.merchant_id = sqlc.arg(merchant_id)::uuid
  AND (sqlc.narg(customer_id)::uuid IS NULL OR o.customer_id = sqlc.narg(customer_id)::uuid)
  AND (sqlc.narg(status)::text IS NULL OR o.status = sqlc.narg(status)::text)
  AND (sqlc.narg(origin)::text IS NULL OR o.origin = sqlc.narg(origin)::text)
  AND (sqlc.narg(subscription_id)::uuid IS NULL OR o.subscription_id = sqlc.narg(subscription_id)::uuid)
  AND (sqlc.narg(overdue_at)::timestamptz IS NULL OR (o.status = 'open' AND o.due_at < sqlc.narg(overdue_at)::timestamptz))
  AND (sqlc.narg(price_id)::uuid IS NULL OR EXISTS (
        SELECT 1 FROM billing.order_lines l
        WHERE l.merchant_id = o.merchant_id AND l.order_id = o.id AND l.price_id = sqlc.narg(price_id)::uuid))
  AND (sqlc.narg(after_created_at)::timestamptz IS NULL
       OR (o.created_at, o.id) < (sqlc.narg(after_created_at)::timestamptz, sqlc.narg(after_id)::uuid))
ORDER BY o.created_at DESC, o.id DESC
LIMIT sqlc.arg(row_limit)::int;

-- name: ListOrdersByIDs :many
-- The merchant's named orders, newest first.
SELECT * FROM billing.orders
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = ANY(sqlc.arg(ids)::uuid[])
ORDER BY created_at DESC, id DESC;

-- name: GetOrderLine :one
SELECT * FROM billing.order_lines
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid;

-- name: GetOrderLineBySubscription :one
-- The line of order that produced a subscription; its renewal orders' lines
-- name it too.
SELECT * FROM billing.order_lines
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND order_id = sqlc.arg(order_id)::uuid
  AND subscription_id = sqlc.arg(subscription_id)::uuid;

-- name: ListOrderLines :many
SELECT * FROM billing.order_lines
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND order_id = ANY(sqlc.arg(order_ids)::uuid[])
ORDER BY order_id, "position";

-- name: SetOrderLineProduced :exec
UPDATE billing.order_lines
SET subscription_id = COALESCE(sqlc.narg(subscription_id)::uuid, subscription_id),
    product_access_id = COALESCE(sqlc.narg(product_access_id)::uuid, product_access_id)
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid;

-- An order whose one line is a subscription: its payment is that
-- subscription's first, so refunds and disputes of it reach the subscription.
-- name: SetPaymentSubscription :exec
UPDATE billing.payments SET subscription_id = sqlc.arg(subscription_id)::uuid,
    metadata = COALESCE(metadata, '{}'::jsonb) || jsonb_build_object('period_start', sqlc.arg(period_start)::text)
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid AND subscription_id IS NULL AND deleted_at IS NULL;

-- An attempt starts on an open order: one that takes payment, or awaits the
-- customer's action on an attempt that has since ended.
-- name: StartOrderAttempt :execrows
UPDATE billing.orders
SET payment_status = 'requires_payment_method', attempt_id = sqlc.arg(attempt_id)::uuid, payment_method_id = sqlc.narg(payment_method_id)::uuid,
    psp_id = sqlc.arg(psp_id)::uuid, last_payment_error = NULL, updated_at = sqlc.arg(now)::timestamptz
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid AND status = 'open';

-- name: SetOrderPending :execrows
-- The live attempt awaits the customer (the order stays open) or the
-- provider (the order is processing).
UPDATE billing.orders
SET payment_status = sqlc.arg(payment_status)::text,
    status = CASE sqlc.arg(payment_status)::text WHEN 'processing' THEN 'processing' ELSE 'open' END,
    updated_at = sqlc.arg(now)::timestamptz
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid
  AND attempt_id = sqlc.arg(attempt_id)::uuid AND status IN ('open', 'processing')
  AND sqlc.arg(payment_status)::text IN ('requires_action', 'processing');

-- name: SetOrderDeclined :execrows
UPDATE billing.orders
SET status = 'open', payment_status = 'requires_payment_method', last_payment_error = sqlc.arg(last_payment_error)::jsonb,
    updated_at = sqlc.arg(now)::timestamptz
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid
  AND attempt_id = sqlc.arg(attempt_id)::uuid AND status IN ('open', 'processing');

-- name: SetOrderPaymentMethod :execrows
-- The card a new card's charge saved, once it exists.
UPDATE billing.orders
SET payment_method_id = sqlc.arg(payment_method_id)::uuid, updated_at = sqlc.arg(now)::timestamptz
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid;

-- name: SetOrderComplete :execrows
UPDATE billing.orders
SET status = 'complete', payment_status = 'succeeded', number = sqlc.arg(number)::bigint, payment_id = sqlc.narg(payment_id)::uuid,
    completed_at = sqlc.arg(now)::timestamptz, last_payment_error = NULL, updated_at = sqlc.arg(now)::timestamptz
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid AND status <> 'complete';

-- name: SetOrderLatePayment :execrows
-- Money moved on a closed order whose claims were taken meanwhile: the
-- payment is recorded, the order stays closed and the payment is refunded.
UPDATE billing.orders
SET payment_id = sqlc.arg(payment_id)::uuid, payment_status = 'succeeded', updated_at = sqlc.arg(now)::timestamptz
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid
  AND status IN ('canceled', 'expired') AND payment_id IS NULL;

-- name: CloseOrder :execrows
UPDATE billing.orders
SET status = sqlc.arg(status)::text, payment_status = 'requires_payment_method',
    canceled_at = CASE WHEN sqlc.arg(status)::text = 'canceled' THEN sqlc.arg(now)::timestamptz ELSE canceled_at END,
    expired_at = CASE WHEN sqlc.arg(status)::text = 'expired' THEN sqlc.arg(now)::timestamptz ELSE expired_at END,
    updated_at = sqlc.arg(now)::timestamptz
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid
  AND collection_owner = 'engine' AND status = 'open' AND sqlc.arg(status)::text IN ('canceled', 'expired');

-- name: ListOrderSweepMerchants :many
-- The sweep's work queue: merchants with a live order past its expiry, or an
-- unpaid closed order past retention.
SELECT merchant_id FROM (
    SELECT merchant_id FROM billing.orders
    WHERE collection_owner = 'engine' AND status = 'open' AND expires_at <= sqlc.arg(now)::timestamptz
    UNION
    SELECT merchant_id FROM billing.orders
    WHERE collection_owner = 'engine' AND status IN ('canceled', 'expired') AND payment_id IS NULL AND attempt_id IS NULL
      AND COALESCE(canceled_at, expired_at) < sqlc.arg(purge_before)::timestamptz
) due
LIMIT sqlc.arg(merchant_limit)::int;

-- name: ListExpiredOrders :many
SELECT id FROM billing.orders
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND collection_owner = 'engine' AND status = 'open'
  AND expires_at <= sqlc.arg(now)::timestamptz
ORDER BY expires_at
LIMIT sqlc.arg(row_limit)::int;

-- name: DeleteClosedUnpaidOrders :execrows
-- Retention: an unpaid closed order that never started a payment attempt goes
-- 90 days after it closed; its lines and claims go with it.
DELETE FROM billing.orders o
WHERE o.ctid IN (
    SELECT c.ctid FROM billing.orders c
    WHERE c.merchant_id = sqlc.arg(merchant_id)::uuid AND c.collection_owner = 'engine' AND c.status IN ('canceled', 'expired')
      AND c.payment_id IS NULL AND c.attempt_id IS NULL
      AND COALESCE(c.canceled_at, c.expired_at) < sqlc.arg(before)::timestamptz
      AND NOT EXISTS (SELECT 1 FROM billing.order_attempts a WHERE a.merchant_id = c.merchant_id AND a.order_id = c.id)
    LIMIT sqlc.arg(row_limit)::int
);

-- name: ClaimOwnership :execrows
INSERT INTO billing.ownership_claims (merchant_id, customer_id, claim_key, order_id, holder_type, holder_id, created_at, updated_at)
VALUES (sqlc.arg(merchant_id)::uuid, sqlc.arg(customer_id)::uuid, sqlc.arg(claim_key)::text, sqlc.arg(order_id)::uuid,
        'order', sqlc.arg(order_id)::uuid, sqlc.arg(now)::timestamptz, sqlc.arg(now)::timestamptz)
ON CONFLICT (merchant_id, customer_id, claim_key) DO NOTHING;

-- name: GetOwnershipClaim :one
SELECT * FROM billing.ownership_claims
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND customer_id = sqlc.arg(customer_id)::uuid
  AND claim_key = sqlc.arg(claim_key)::text;

-- name: ListOwnershipClaims :many
SELECT * FROM billing.ownership_claims
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND customer_id = sqlc.arg(customer_id)::uuid
  AND claim_key = ANY(sqlc.arg(claim_keys)::text[]);

-- name: HandOverOwnershipClaim :execrows
UPDATE billing.ownership_claims
SET holder_type = sqlc.arg(holder_type)::text, holder_id = sqlc.arg(holder_id)::uuid, updated_at = sqlc.arg(now)::timestamptz
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND customer_id = sqlc.arg(customer_id)::uuid
  AND claim_key = sqlc.arg(claim_key)::text AND order_id = sqlc.arg(order_id)::uuid AND holder_type = 'order';

-- name: ReleaseOrderClaims :execrows
DELETE FROM billing.ownership_claims
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND customer_id = sqlc.arg(customer_id)::uuid
  AND order_id = sqlc.arg(order_id)::uuid AND holder_type = 'order';

-- name: NextOrderNumber :one
SELECT nextval('billing.order_number_seq'::regclass)::bigint AS number;

-- name: ListLiveOwnership :many
-- What a customer holds of the given products or tier groups: live
-- subscriptions (a canceled one while still paid through or while its
-- provider schedule may still bill), and product access that never ends.
SELECT 'subscription'::text AS holder_type, s.id AS holder_id, s.product_id, s.tier_group,
       s.status, s.current_period_ends_at
FROM billing.subscriptions s
WHERE s.merchant_id = sqlc.arg(merchant_id)::uuid AND s.customer_id = sqlc.arg(customer_id)::uuid
  AND s.deleted_at IS NULL
  AND (s.product_id = ANY(sqlc.arg(product_ids)::uuid[]) OR s.tier_group = ANY(sqlc.arg(tier_groups)::text[]))
  AND (s.status <> 'canceled' OR s.current_period_ends_at > sqlc.arg(now)::timestamptz OR s.deletion_scheduled_at IS NOT NULL)
UNION ALL
SELECT 'product_access'::text, a.id, a.product_id, NULL::text, 'active'::text, a.ends_at
FROM billing.product_access a
WHERE a.merchant_id = sqlc.arg(merchant_id)::uuid AND a.customer_id = sqlc.arg(customer_id)::uuid
  AND a.product_id = ANY(sqlc.arg(product_ids)::uuid[])
  AND a.source_type IN ('purchase', 'grant') AND a.ends_at IS NULL AND a.revoked_at IS NULL AND a.deleted_at IS NULL
  AND a.starts_at <= sqlc.arg(now)::timestamptz;

-- name: CreateOrderAttempt :exec
-- A card attempt starts created; a Solana one requires_action, with its
-- reference.
INSERT INTO billing.order_attempts (
    id, merchant_id, customer_id, order_id, rail, status, reference, amount, currency,
    expires_at, rail_state, psp_id, created_at, updated_at
) VALUES (
    sqlc.arg(id)::uuid, sqlc.arg(merchant_id)::uuid, sqlc.arg(customer_id)::uuid, sqlc.arg(order_id)::uuid,
    sqlc.arg(rail)::text, sqlc.arg(status)::text, sqlc.narg(reference)::text, sqlc.arg(amount)::bigint, sqlc.arg(currency)::text,
    sqlc.arg(expires_at)::timestamptz, sqlc.arg(rail_state)::jsonb, sqlc.arg(psp_id)::uuid,
    sqlc.arg(now)::timestamptz, sqlc.arg(now)::timestamptz
);

-- name: GetOrderAttempt :one
SELECT * FROM billing.order_attempts
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid AND order_id IS NOT NULL AND deleted_at IS NULL;

-- name: SetOrderAttemptStatus :execrows
-- A terminal attempt never moves again.
UPDATE billing.order_attempts
SET status = sqlc.arg(status)::text,
    payment_id = COALESCE(sqlc.narg(payment_id)::uuid, payment_id),
    transaction_id = COALESCE(sqlc.narg(transaction_id)::text, transaction_id),
    rail_state = COALESCE(rail_state, '{}'::jsonb) || COALESCE(sqlc.narg(rail_state)::jsonb, '{}'::jsonb),
    updated_at = sqlc.arg(now)::timestamptz
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid AND order_id IS NOT NULL
  AND status IN ('created', 'requires_action', 'processing') AND deleted_at IS NULL;

-- name: EnqueueOrderHostEvent :execrows
INSERT INTO billing.host_outbox (merchant_id, event_type, subject_type, subject_id, amount, currency, occurred_at, data, dedupe_key)
VALUES (sqlc.arg(merchant_id)::uuid, sqlc.arg(event_type)::text, 'order', sqlc.arg(order_id)::uuid,
        sqlc.arg(amount)::bigint, sqlc.arg(currency)::text, sqlc.arg(occurred_at)::timestamptz,
        sqlc.arg(data)::jsonb, sqlc.arg(dedupe_key)::text)
ON CONFLICT (merchant_id, dedupe_key) DO NOTHING;
