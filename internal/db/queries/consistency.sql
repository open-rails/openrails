-- #511 CON plane queries (conPass). Each takes an OPTIONAL customer_id: when set
-- (inline Converge(customer)), the scan is restricted to that one customer's rows
-- so an after-every-mutation invocation is O(customer), not O(merchant); when
-- NULL (the merchant-wide sweep) it scans the whole merchant. All are RLS-scoped.
-- These replace the retired internal/audit checks (#511 Phase F hard cut).

-- name: ConOrphanEntitlementSubscriptionSource :many
SELECT ent.id AS ent_id, ent.customer_id::text AS user_id, ent.entitlement, ent.source_type, ent.source_id
FROM openrails.entitlements ent
LEFT JOIN openrails.subscriptions sub ON ent.source_id = sub.id
WHERE ent.source_type = 'subscription'
  AND ent.source_id IS NOT NULL
  AND ent.deleted_at IS NULL
  AND sub.id IS NULL
  AND (sqlc.narg(customer_id)::uuid IS NULL OR ent.customer_id = sqlc.narg(customer_id)::uuid);

-- name: ConOrphanEntitlementPaymentSource :many
SELECT ent.id AS ent_id, ent.customer_id::text AS user_id, ent.entitlement, ent.source_type, ent.source_id
FROM openrails.entitlements ent
LEFT JOIN openrails.payments purch ON ent.source_id = purch.id
WHERE ent.source_type = 'one_off'
  AND ent.source_id IS NOT NULL
  AND ent.deleted_at IS NULL
  AND purch.id IS NULL
  AND (sqlc.narg(customer_id)::uuid IS NULL OR ent.customer_id = sqlc.narg(customer_id)::uuid);


-- name: ConDuplicateChargesSamePeriod :many
-- More than one settled, non-refunded charge for the same customer/product/month.
WITH payment_products AS (
    SELECT purch.id, purch.customer_id, purch.amount, purch.purchased_at, price.product_id, prod.key AS product_key
    FROM openrails.payments purch
    JOIN openrails.prices price ON purch.price_id = price.id
    JOIN openrails.products prod ON price.product_id = prod.id
    WHERE purch.amount > 0
      AND purch.refunded_payment_id IS NULL
      AND (sqlc.narg(customer_id)::uuid IS NULL OR purch.customer_id = sqlc.narg(customer_id)::uuid)
)
SELECT
    customer_id::text AS user_id,
    product_id,
    product_key,
    COUNT(*)::int AS count,
    ARRAY_AGG(id ORDER BY purchased_at DESC)::uuid[] AS payment_ids,
    SUM(amount)::bigint AS total_amount,
    MIN(purchased_at)::timestamptz AS first_date,
    MAX(purchased_at)::timestamptz AS last_date
FROM payment_products
GROUP BY customer_id, product_id, product_key, DATE_TRUNC('month', purchased_at)
HAVING COUNT(*) > 1;
