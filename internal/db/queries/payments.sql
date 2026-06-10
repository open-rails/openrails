-- billing.payments — immutable payment event log.
--
-- Insert semantics replicate the bun-era model tags: a zero value on a
-- column with a default (status, currency, purchased_at, created_at) falls
-- back to that default via COALESCE/NULLIF, matching bun's
-- "zero + default tag => DEFAULT" rule. tenant_id is never written by the
-- app (column default + RLS WITH CHECK own it).

-- name: CreatePayment :execrows
INSERT INTO billing.payments (
    id, price_id, processor, transaction_id, amount, list_amount, currency,
    status, subscription_id, refunded_payment_id, discount_code,
    discount_reason, discount_metadata, entitlements_spec_snapshot,
    credits_spec_snapshot, metadata, purchased_at, created_at, card_brand,
    card_last4, tenant_subject_id
) VALUES (
    $1, $2, $3, $4, $5, $6,
    COALESCE(NULLIF(sqlc.arg(currency)::text, ''), 'usd'),
    COALESCE(NULLIF(sqlc.arg(status)::text, ''), 'completed')::billing.purchase_status,
    sqlc.narg(subscription_id), sqlc.narg(refunded_payment_id),
    sqlc.narg(discount_code), sqlc.narg(discount_reason),
    sqlc.narg(discount_metadata), sqlc.narg(entitlements_spec_snapshot),
    sqlc.narg(credits_spec_snapshot), sqlc.narg(metadata),
    COALESCE(NULLIF(sqlc.arg(purchased_at)::timestamptz, '0001-01-01 00:00:00+00'::timestamptz), now()),
    COALESCE(NULLIF(sqlc.arg(created_at)::timestamptz, '0001-01-01 00:00:00+00'::timestamptz), now()),
    sqlc.narg(card_brand), sqlc.narg(card_last4), sqlc.arg(tenant_subject_id)
);

-- name: CreatePaymentIfNotExists :execrows
INSERT INTO billing.payments (
    id, price_id, processor, transaction_id, amount, list_amount, currency,
    status, subscription_id, refunded_payment_id, discount_code,
    discount_reason, discount_metadata, entitlements_spec_snapshot,
    credits_spec_snapshot, metadata, purchased_at, created_at, card_brand,
    card_last4, tenant_subject_id
) VALUES (
    $1, $2, $3, $4, $5, $6,
    COALESCE(NULLIF(sqlc.arg(currency)::text, ''), 'usd'),
    COALESCE(NULLIF(sqlc.arg(status)::text, ''), 'completed')::billing.purchase_status,
    sqlc.narg(subscription_id), sqlc.narg(refunded_payment_id),
    sqlc.narg(discount_code), sqlc.narg(discount_reason),
    sqlc.narg(discount_metadata), sqlc.narg(entitlements_spec_snapshot),
    sqlc.narg(credits_spec_snapshot), sqlc.narg(metadata),
    COALESCE(NULLIF(sqlc.arg(purchased_at)::timestamptz, '0001-01-01 00:00:00+00'::timestamptz), now()),
    COALESCE(NULLIF(sqlc.arg(created_at)::timestamptz, '0001-01-01 00:00:00+00'::timestamptz), now()),
    sqlc.narg(card_brand), sqlc.narg(card_last4), sqlc.arg(tenant_subject_id)
)
ON CONFLICT (tenant_id, processor, transaction_id) DO NOTHING;

-- name: GetPaymentByID :one
SELECT * FROM billing.payments WHERE id = $1;

-- name: GetPaymentWithPriceProduct :one
SELECT sqlc.embed(purch), sqlc.embed(p), sqlc.embed(prod)
FROM billing.payments purch
JOIN billing.prices p ON p.id = purch.price_id
JOIN billing.products prod ON prod.id = p.product_id
WHERE purch.id = $1;

-- name: ListRefundsForPayment :many
SELECT * FROM billing.payments
WHERE refunded_payment_id = $1
ORDER BY created_at DESC;

-- name: ListPaymentsByTenantSubject :many
SELECT * FROM billing.payments purch
WHERE purch.tenant_subject_id = $1
  AND COALESCE(purch.metadata ->> 'nmi_subscription_order_id', '') = ''
ORDER BY purch.purchased_at DESC;

-- name: GetPaymentByTransactionID :one
SELECT * FROM billing.payments purch
WHERE purch.processor = $1 AND purch.transaction_id = $2;

-- name: ListPaymentsByPriceID :many
SELECT * FROM billing.payments purch
WHERE purch.price_id = $1
  AND COALESCE(purch.metadata ->> 'nmi_subscription_order_id', '') = ''
ORDER BY purch.purchased_at DESC;

-- name: ListPaymentsByProcessor :many
SELECT * FROM billing.payments purch
WHERE purch.processor = $1
  AND COALESCE(purch.metadata ->> 'nmi_subscription_order_id', '') = ''
ORDER BY purch.purchased_at DESC;

-- name: DeletePayment :execrows
DELETE FROM billing.payments WHERE id = $1;

-- name: ListRefundRowsForTotal :many
SELECT amount, status FROM billing.payments
WHERE refunded_payment_id = $1;

-- name: LinkRefundedPayment :execrows
UPDATE billing.payments
SET refunded_payment_id = $2
WHERE id = $1 AND refunded_payment_id IS NULL;

-- name: GetRefundByAdminIdempotencyKey :one
SELECT * FROM billing.payments purch
WHERE purch.refunded_payment_id = $1
  AND purch.metadata ->> 'admin_refund_idempotency_key' = sqlc.arg(idem_key)::text
LIMIT 1;

-- name: CompleteRefundReservation :execrows
UPDATE billing.payments
SET transaction_id = $2, status = 'completed', metadata = $3
WHERE id = $1
  AND refunded_payment_id IS NOT NULL
  AND amount < 0
  AND status = 'pending';

-- name: GetPaymentByMetadataValue :one
SELECT * FROM billing.payments purch
WHERE purch.metadata ->> sqlc.arg(key)::text = sqlc.arg(value)::text
LIMIT 1;

-- name: CompleteProviderAttempt :execrows
UPDATE billing.payments
SET transaction_id = $2, status = 'completed', metadata = $3
WHERE id = $1
  AND amount > 0
  AND status = 'pending';

-- name: CompleteProviderAttemptInPlace :execrows
UPDATE billing.payments
SET metadata = $2
WHERE id = $1
  AND amount > 0
  AND status = 'pending';

-- name: CountPaymentsByTenantSubject :one
SELECT count(*) FROM billing.payments purch
WHERE purch.tenant_subject_id = $1
  AND COALESCE(purch.metadata ->> 'nmi_subscription_order_id', '') = '';

-- name: ListPaymentsByTenantSubjectPaged :many
SELECT * FROM billing.payments purch
WHERE purch.tenant_subject_id = $1
  AND COALESCE(purch.metadata ->> 'nmi_subscription_order_id', '') = ''
ORDER BY purch.purchased_at DESC
LIMIT sqlc.arg(page_limit)::int OFFSET sqlc.arg(page_offset)::int;

-- name: GetLatestPaymentByTenantSubjectProcessor :one
SELECT * FROM billing.payments purch
WHERE purch.tenant_subject_id = $1
  AND purch.processor = $2
  AND COALESCE(purch.metadata ->> 'nmi_subscription_order_id', '') = ''
ORDER BY purch.purchased_at DESC
LIMIT 1;

-- name: GetLatestPaymentBySubscriptionID :one
SELECT * FROM billing.payments purch
WHERE purch.subscription_id = $1
ORDER BY purch.purchased_at DESC
LIMIT 1;

-- name: GetLatestChargeBySubscriptionID :one
SELECT * FROM billing.payments purch
WHERE purch.subscription_id = $1
  AND purch.amount > 0
  AND COALESCE(purch.status::text, 'completed') = 'completed'
ORDER BY purch.purchased_at DESC
LIMIT 1;

-- name: CountPaymentOutcomesBySubjectProcessor :one
SELECT
    count(*) FILTER (WHERE purch.amount > 0
        AND COALESCE(purch.status::text, 'completed') = 'completed')::bigint AS successful,
    count(*) FILTER (WHERE COALESCE(purch.status::text, 'completed') = 'failed')::bigint AS failed
FROM billing.payments purch
WHERE purch.tenant_subject_id = $1
  AND purch.processor = $2
  AND COALESCE(purch.metadata ->> 'nmi_subscription_order_id', '') = '';

-- name: MarkPaymentFailed :exec
UPDATE billing.payments SET status = 'failed' WHERE id = $1;

-- name: CountPaymentsFiltered :one
SELECT count(*) FROM billing.payments purch
WHERE COALESCE(purch.metadata ->> 'nmi_subscription_order_id', '') = ''
  AND (sqlc.narg(tenant_subject_id)::uuid IS NULL OR purch.tenant_subject_id = sqlc.narg(tenant_subject_id)::uuid)
  AND (sqlc.narg(price_id)::uuid IS NULL OR purch.price_id = sqlc.narg(price_id)::uuid)
  AND (sqlc.narg(subscription_id)::uuid IS NULL OR purch.subscription_id = sqlc.narg(subscription_id)::uuid)
  AND (sqlc.narg(processor)::text IS NULL OR purch.processor::text = sqlc.narg(processor)::text)
  AND (sqlc.narg(transaction_id)::text IS NULL OR purch.transaction_id = sqlc.narg(transaction_id)::text)
  AND (sqlc.narg(purchased_after)::timestamptz IS NULL OR purch.purchased_at >= sqlc.narg(purchased_after)::timestamptz)
  AND (sqlc.narg(purchased_before)::timestamptz IS NULL OR purch.purchased_at <= sqlc.narg(purchased_before)::timestamptz)
  AND (sqlc.narg(min_amount)::bigint IS NULL OR purch.amount >= sqlc.narg(min_amount)::bigint)
  AND (sqlc.narg(max_amount)::bigint IS NULL OR purch.amount <= sqlc.narg(max_amount)::bigint)
  AND (NOT sqlc.arg(refunds_only)::boolean OR purch.refunded_payment_id IS NOT NULL);

-- name: ListPaymentsFiltered :many
-- Sorting is static SQL over a validated (sort_by, sort_desc) pair via the
-- CASE pattern — no identifier interpolation (#334 escape-hatch rule).
SELECT * FROM billing.payments purch
WHERE COALESCE(purch.metadata ->> 'nmi_subscription_order_id', '') = ''
  AND (sqlc.narg(tenant_subject_id)::uuid IS NULL OR purch.tenant_subject_id = sqlc.narg(tenant_subject_id)::uuid)
  AND (sqlc.narg(price_id)::uuid IS NULL OR purch.price_id = sqlc.narg(price_id)::uuid)
  AND (sqlc.narg(subscription_id)::uuid IS NULL OR purch.subscription_id = sqlc.narg(subscription_id)::uuid)
  AND (sqlc.narg(processor)::text IS NULL OR purch.processor::text = sqlc.narg(processor)::text)
  AND (sqlc.narg(transaction_id)::text IS NULL OR purch.transaction_id = sqlc.narg(transaction_id)::text)
  AND (sqlc.narg(purchased_after)::timestamptz IS NULL OR purch.purchased_at >= sqlc.narg(purchased_after)::timestamptz)
  AND (sqlc.narg(purchased_before)::timestamptz IS NULL OR purch.purchased_at <= sqlc.narg(purchased_before)::timestamptz)
  AND (sqlc.narg(min_amount)::bigint IS NULL OR purch.amount >= sqlc.narg(min_amount)::bigint)
  AND (sqlc.narg(max_amount)::bigint IS NULL OR purch.amount <= sqlc.narg(max_amount)::bigint)
  AND (NOT sqlc.arg(refunds_only)::boolean OR purch.refunded_payment_id IS NOT NULL)
ORDER BY
    CASE WHEN sqlc.arg(sort_by)::text = 'amount'       AND NOT sqlc.arg(sort_desc)::boolean THEN purch.amount END ASC,
    CASE WHEN sqlc.arg(sort_by)::text = 'amount'       AND sqlc.arg(sort_desc)::boolean     THEN purch.amount END DESC,
    CASE WHEN sqlc.arg(sort_by)::text = 'purchased_at' AND NOT sqlc.arg(sort_desc)::boolean THEN purch.purchased_at END ASC,
    CASE WHEN sqlc.arg(sort_by)::text = 'purchased_at' AND sqlc.arg(sort_desc)::boolean     THEN purch.purchased_at END DESC,
    CASE WHEN sqlc.arg(sort_by)::text = 'created_at'   AND NOT sqlc.arg(sort_desc)::boolean THEN purch.created_at END ASC,
    CASE WHEN sqlc.arg(sort_by)::text = 'created_at'   AND sqlc.arg(sort_desc)::boolean     THEN purch.created_at END DESC
LIMIT sqlc.arg(page_limit)::int OFFSET sqlc.arg(page_offset)::int;

-- name: ListPaymentsByIDs :many
SELECT * FROM billing.payments WHERE id = ANY(sqlc.arg(ids)::uuid[]);
