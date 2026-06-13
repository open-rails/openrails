-- openrails.payments — immutable payment event log.
--
-- Insert semantics replicate the bun-era model tags: a zero value on a
-- column with a default (status, currency, purchased_at, created_at) falls
-- back to that default via COALESCE/NULLIF, matching bun's
-- "zero + default tag => DEFAULT" rule. tenant_id is written explicitly
-- (RLS WITH CHECK still enforces it).

-- name: CreatePayment :execrows
INSERT INTO openrails.payments (
    id, tenant_id, price_id, processor, transaction_id, amount, list_amount, currency,
    status, subscription_id, refunded_payment_id, discount_code,
    discount_reason, discount_metadata, entitlements_spec_snapshot,
    credits_spec_snapshot, metadata, purchased_at, created_at, card_brand,
    card_last4, tenant_subject_id
) VALUES (
    $1, sqlc.arg(tenant_id)::uuid, $2, $3, $4, $5, $6,
    COALESCE(NULLIF(sqlc.arg(currency)::text, ''), 'usd'),
    COALESCE(NULLIF(sqlc.arg(status)::text, ''), 'completed')::openrails.purchase_status,
    sqlc.narg(subscription_id), sqlc.narg(refunded_payment_id),
    sqlc.narg(discount_code), sqlc.narg(discount_reason),
    sqlc.narg(discount_metadata), sqlc.narg(entitlements_spec_snapshot),
    sqlc.narg(credits_spec_snapshot), sqlc.narg(metadata),
    COALESCE(NULLIF(sqlc.arg(purchased_at)::timestamptz, '0001-01-01 00:00:00+00'::timestamptz), now()),
    COALESCE(NULLIF(sqlc.arg(created_at)::timestamptz, '0001-01-01 00:00:00+00'::timestamptz), now()),
    sqlc.narg(card_brand), sqlc.narg(card_last4), sqlc.arg(tenant_subject_id)
);

-- name: CreatePaymentIfNotExists :execrows
INSERT INTO openrails.payments (
    id, tenant_id, price_id, processor, transaction_id, amount, list_amount, currency,
    status, subscription_id, refunded_payment_id, discount_code,
    discount_reason, discount_metadata, entitlements_spec_snapshot,
    credits_spec_snapshot, metadata, purchased_at, created_at, card_brand,
    card_last4, tenant_subject_id
) VALUES (
    $1, sqlc.arg(tenant_id)::uuid, $2, $3, $4, $5, $6,
    COALESCE(NULLIF(sqlc.arg(currency)::text, ''), 'usd'),
    COALESCE(NULLIF(sqlc.arg(status)::text, ''), 'completed')::openrails.purchase_status,
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
SELECT * FROM openrails.payments WHERE id = $1;

-- name: GetPaymentWithPriceProduct :one
SELECT sqlc.embed(purch), sqlc.embed(p), sqlc.embed(prod)
FROM openrails.payments purch
JOIN openrails.prices p ON p.id = purch.price_id
JOIN openrails.products prod ON prod.id = p.product_id
WHERE purch.id = $1;

-- name: ListRefundsForPayment :many
SELECT * FROM openrails.payments
WHERE refunded_payment_id = $1
ORDER BY created_at DESC;

-- name: ListPaymentsByTenantSubject :many
SELECT * FROM openrails.payments purch
WHERE purch.tenant_subject_id = $1
  AND COALESCE(purch.metadata ->> 'nmi_subscription_order_id', '') = ''
ORDER BY purch.purchased_at DESC;

-- name: GetPaymentByTransactionID :one
SELECT * FROM openrails.payments purch
WHERE purch.processor = $1 AND purch.transaction_id = $2;

-- name: ListPaymentsByPriceID :many
SELECT * FROM openrails.payments purch
WHERE purch.price_id = $1
  AND COALESCE(purch.metadata ->> 'nmi_subscription_order_id', '') = ''
ORDER BY purch.purchased_at DESC;

-- name: ListPaymentsByProcessor :many
SELECT * FROM openrails.payments purch
WHERE purch.processor = $1
  AND COALESCE(purch.metadata ->> 'nmi_subscription_order_id', '') = ''
ORDER BY purch.purchased_at DESC;

-- name: DeletePayment :execrows
DELETE FROM openrails.payments WHERE id = $1;

-- name: ListRefundRowsForTotal :many
SELECT amount, status FROM openrails.payments
WHERE refunded_payment_id = $1;

-- name: LinkRefundedPayment :execrows
UPDATE openrails.payments
SET refunded_payment_id = $2
WHERE id = $1 AND refunded_payment_id IS NULL;

-- name: GetRefundByAdminIdempotencyKey :one
SELECT * FROM openrails.payments purch
WHERE purch.refunded_payment_id = $1
  AND purch.metadata ->> 'admin_refund_idempotency_key' = sqlc.arg(idem_key)::text
LIMIT 1;

-- name: CompleteRefundReservation :execrows
UPDATE openrails.payments
SET transaction_id = $2, status = 'completed', metadata = $3
WHERE id = $1
  AND refunded_payment_id IS NOT NULL
  AND amount < 0
  AND status = 'pending';

-- name: GetPaymentByMetadataValue :one
SELECT * FROM openrails.payments purch
WHERE purch.metadata ->> sqlc.arg(key)::text = sqlc.arg(value)::text
LIMIT 1;

-- name: CompleteProviderAttempt :execrows
UPDATE openrails.payments
SET transaction_id = $2, status = 'completed', metadata = $3
WHERE id = $1
  AND amount > 0
  AND status = 'pending';

-- Resolves a provider attempt row whose real payment is recorded separately
-- (NMI subscription checkout): the row keeps its synthetic transaction_id and
-- is excluded from listings via the nmi_subscription_order_id metadata key,
-- but its status must still reach a terminal state — leaving it 'pending'
-- forever reads as a stuck payment.
-- name: CompleteProviderAttemptInPlace :execrows
UPDATE openrails.payments
SET metadata = $2, status = 'completed'
WHERE id = $1
  AND amount > 0
  AND status = 'pending';

-- name: CountPaymentsByTenantSubject :one
SELECT count(*) FROM openrails.payments purch
WHERE purch.tenant_subject_id = $1
  AND COALESCE(purch.metadata ->> 'nmi_subscription_order_id', '') = '';

-- name: ListPaymentsByTenantSubjectPaged :many
SELECT * FROM openrails.payments purch
WHERE purch.tenant_subject_id = $1
  AND COALESCE(purch.metadata ->> 'nmi_subscription_order_id', '') = ''
ORDER BY purch.purchased_at DESC
LIMIT sqlc.arg(page_limit)::int OFFSET sqlc.arg(page_offset)::int;

-- name: GetLatestPaymentByTenantSubjectProcessor :one
SELECT * FROM openrails.payments purch
WHERE purch.tenant_subject_id = $1
  AND purch.processor = $2
  AND COALESCE(purch.metadata ->> 'nmi_subscription_order_id', '') = ''
ORDER BY purch.purchased_at DESC
LIMIT 1;

-- name: GetLatestPaymentBySubscriptionID :one
SELECT * FROM openrails.payments purch
WHERE purch.subscription_id = $1
ORDER BY purch.purchased_at DESC
LIMIT 1;

-- name: GetLatestChargeBySubscriptionID :one
SELECT * FROM openrails.payments purch
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
FROM openrails.payments purch
WHERE purch.tenant_subject_id = $1
  AND purch.processor = $2
  AND COALESCE(purch.metadata ->> 'nmi_subscription_order_id', '') = '';

-- name: MarkPaymentFailed :exec
UPDATE openrails.payments SET status = 'failed' WHERE id = $1;

-- name: CountPaymentsFiltered :one
SELECT count(*) FROM openrails.payments purch
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
SELECT * FROM openrails.payments purch
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
SELECT * FROM openrails.payments WHERE id = ANY(sqlc.arg(ids)::uuid[]);

-- name: MatchChargebackPayments :many
-- NMI chargeback reconciliation (webhooks/nmi.go): candidate subscription
-- payments matched by amount + card last4 within ±7d of the chargeback date,
-- closest-in-time first. LIMIT 2 so the caller can detect ambiguity.
SELECT p.id AS payment_id,
       p.transaction_id AS payment_transaction_id,
       p.subscription_id AS subscription_id,
       COALESCE(sub.processor_subscription_id, '')::text AS processor_subscription_id,
       p.tenant_subject_id::text AS user_id,
       p.amount AS amount_cents,
       p.currency AS currency,
       p.purchased_at AS purchased_at,
       COALESCE(pm.last_four, '')::text AS card_last4
FROM openrails.payments p
JOIN openrails.subscriptions sub ON sub.id = p.subscription_id
LEFT JOIN openrails.payment_methods pm ON pm.id = sub.payment_method_id
WHERE p.subscription_id IS NOT NULL
  AND p.processor = sqlc.arg(processor)
  AND sub.processor::text = p.processor::text
  AND p.amount > 0
  AND p.amount = sqlc.arg(amount_cents)
  AND RIGHT(regexp_replace(COALESCE(pm.last_four, ''), '[^0-9]', '', 'g'), 4) = sqlc.arg(last4)::text
  AND p.purchased_at >= sqlc.arg(from_at)::timestamptz
  AND p.purchased_at <= sqlc.arg(to_at)::timestamptz
ORDER BY ABS(EXTRACT(EPOCH FROM (p.purchased_at - sqlc.arg(target_at)::timestamptz))) ASC,
         p.purchased_at DESC
LIMIT 2;

-- name: SnapshotPaymentCards :exec
-- Backfill a card snapshot onto Stripe payments that still lack one.
UPDATE openrails.payments
SET card_brand = sqlc.arg(card_brand)::text,
    card_last4 = sqlc.arg(card_last4)::text
WHERE processor = 'stripe'
  AND transaction_id = ANY(sqlc.arg(transaction_ids)::text[])
  AND card_last4 IS NULL;

-- name: MergeStripePaymentMetadata :exec
UPDATE openrails.payments
SET metadata = COALESCE(metadata, '{}'::jsonb) || sqlc.arg(patch)::jsonb
WHERE processor = 'stripe'
  AND transaction_id = sqlc.arg(transaction_id);

-- name: GetStripeAliasCardSnapshot :one
SELECT card_brand, card_last4 FROM openrails.payments
WHERE processor = 'stripe'
  AND transaction_id = ANY(sqlc.arg(transaction_ids)::text[])
  AND card_last4 IS NOT NULL
LIMIT 1;
