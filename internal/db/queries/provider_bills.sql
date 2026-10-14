-- name: LockProviderBillIdentity :exec
SELECT pg_advisory_xact_lock(hashtextextended(sqlc.arg(merchant_id)::text || ':' || sqlc.arg(psp_id)::text || ':' || sqlc.arg(source_kind)::text || ':' || sqlc.arg(source_key)::text, 0));

-- name: GetProviderBill :one
SELECT * FROM billing.provider_bills
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND psp_id = sqlc.arg(psp_id)::uuid
  AND source_kind = sqlc.arg(source_kind)::text AND source_key = sqlc.arg(source_key)::text;

-- name: CreateProviderBill :exec
INSERT INTO billing.provider_bills (merchant_id, psp_id, source_kind, source_key, order_id, customer_id, snapshot, provider_reference, created_at)
VALUES (sqlc.arg(merchant_id)::uuid, sqlc.arg(psp_id)::uuid, sqlc.arg(source_kind)::text, sqlc.arg(source_key)::text,
        sqlc.arg(order_id)::uuid, sqlc.arg(customer_id)::uuid, sqlc.arg(snapshot)::jsonb, sqlc.arg(provider_reference)::text, sqlc.arg(now)::timestamptz);

-- name: SetProviderBillSettled :execrows
UPDATE billing.provider_bills SET settlement_source = sqlc.arg(settlement_source)::text, non_cash_amount = sqlc.arg(non_cash_amount)::bigint,
    settled_at = sqlc.arg(now)::timestamptz
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND psp_id = sqlc.arg(psp_id)::uuid
  AND source_kind = sqlc.arg(source_kind)::text AND source_key = sqlc.arg(source_key)::text AND settled_at IS NULL;

-- name: CreateObservedOrder :one
INSERT INTO billing.orders (merchant_id, id, customer_id, origin, status, payment_status, currency, total,
    psp_id, collection_owner, subscription_bill_kind, subscription_id, period_starts_at, period_ends_at, due_at, expires_at, created_at, updated_at)
VALUES (sqlc.arg(merchant_id)::uuid, sqlc.arg(id)::uuid, sqlc.arg(customer_id)::uuid, sqlc.arg(origin)::text,
    'open', 'requires_payment_method', sqlc.arg(currency)::text, sqlc.arg(total)::bigint, sqlc.arg(psp_id)::uuid,
    sqlc.arg(collection_owner)::text, sqlc.narg(subscription_bill_kind)::text, sqlc.narg(subscription_id)::uuid,
    sqlc.narg(period_starts_at)::timestamptz, sqlc.narg(period_ends_at)::timestamptz, sqlc.narg(due_at)::timestamptz,
    sqlc.narg(expires_at)::timestamptz, sqlc.arg(now)::timestamptz, sqlc.arg(now)::timestamptz)
RETURNING *;

-- name: BindObservedOrderOwner :execrows
UPDATE billing.orders o SET collection_owner = sqlc.arg(collection_owner)::text, psp_id = sqlc.arg(psp_id)::uuid
WHERE o.merchant_id = sqlc.arg(merchant_id)::uuid AND o.id = sqlc.arg(order_id)::uuid
  AND (o.psp_id IS NULL OR o.psp_id = sqlc.arg(psp_id)::uuid)
  AND (o.collection_owner = sqlc.arg(collection_owner)::text OR
      (o.origin IN ('customer', 'merchant') AND NOT EXISTS (
       SELECT 1 FROM billing.provider_bills b WHERE b.merchant_id = o.merchant_id AND b.order_id = o.id)));

-- name: CreateObservedOrderLine :exec
INSERT INTO billing.order_lines (merchant_id, id, order_id, customer_id, "position", price_id, product_id, description,
    quantity, unit_amount, amount, ownership, claim_key, billing_interval_hours, access_duration_hours, provider_access_default, subscription_id, source, provider_amount, created_at)
VALUES (sqlc.arg(merchant_id)::uuid, sqlc.arg(id)::uuid, sqlc.arg(order_id)::uuid, sqlc.arg(customer_id)::uuid, sqlc.arg(position)::int,
    sqlc.narg(price_id)::uuid, sqlc.narg(product_id)::uuid, sqlc.arg(description)::text, sqlc.narg(quantity)::int,
    sqlc.arg(unit_amount)::bigint, sqlc.arg(amount)::bigint, sqlc.narg(ownership)::text, sqlc.narg(claim_key)::text,
    sqlc.narg(billing_interval_hours)::int, sqlc.narg(access_duration_hours)::int, sqlc.arg(provider_access_default)::boolean, sqlc.narg(subscription_id)::uuid,
    sqlc.narg(source)::text, true, sqlc.arg(now)::timestamptz);

-- name: ObservedOrderCaptured :one
SELECT COALESCE(sum(amount), 0)::bigint FROM billing.payments
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND order_id = sqlc.arg(order_id)::uuid
  AND refunded_payment_id IS NULL AND money_movement = 'rail' AND amount > 0
  AND status IN ('succeeded', 'refunded') AND deleted_at IS NULL;

-- name: LinkAcceptedRenewalPaymentOrder :execrows
UPDATE billing.payments p SET order_id = sqlc.arg(order_id)::uuid
WHERE p.merchant_id = sqlc.arg(merchant_id)::uuid AND p.id = sqlc.arg(payment_id)::uuid AND p.deleted_at IS NULL
  AND (p.order_id IS NULL OR p.order_id = sqlc.arg(order_id)::uuid)
  AND EXISTS (SELECT 1 FROM billing.orders o WHERE o.merchant_id = p.merchant_id AND o.id = sqlc.arg(order_id)::uuid
    AND o.customer_id = p.customer_id AND o.currency = p.currency AND o.subscription_id = p.subscription_id
    AND o.collection_owner = 'engine');

-- name: PaymentMatchesOrderPrice :one
SELECT EXISTS (SELECT 1 FROM billing.payments p JOIN billing.order_lines l ON l.merchant_id = p.merchant_id AND l.order_id = p.order_id
 WHERE p.merchant_id = sqlc.arg(merchant_id)::uuid AND p.id = sqlc.arg(payment_id)::uuid AND l.price_id = sqlc.arg(price_id)::uuid AND p.deleted_at IS NULL)::boolean;

-- name: ListProviderBillsForOrders :many
SELECT b.order_id, b.psp_id, b.source_kind, b.provider_reference, b.settlement_source, b.non_cash_amount, b.settled_at
FROM unnest(sqlc.arg(order_ids)::uuid[]) AS orders(id)
CROSS JOIN LATERAL (
    SELECT order_id, psp_id, source_kind, source_key, provider_reference, settlement_source, non_cash_amount, settled_at, created_at
    FROM billing.provider_bills
    WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND order_id = orders.id
    ORDER BY created_at, source_kind, source_key LIMIT 100
) b;
