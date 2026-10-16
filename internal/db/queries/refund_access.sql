-- name: MarkPaymentAccessRevoked :exec
-- This local access decision survives later policy changes and receipt replay.
UPDATE billing.payments
SET metadata = coalesce(metadata, '{}'::jsonb) || '{"refund_access_revoked":true}'::jsonb
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(payment_id)::uuid;

-- name: LockPaymentSubscriptions :many
-- A cart payment can fund more than one membership. The caller locks the
-- customer first; all affected memberships follow in a stable order.
SELECT s.id FROM billing.subscriptions s
WHERE s.merchant_id=sqlc.arg(merchant_id)::uuid AND EXISTS (
 SELECT 1 FROM billing.payments p
 LEFT JOIN billing.orders o ON o.merchant_id=p.merchant_id AND o.id=p.order_id
 LEFT JOIN billing.order_lines l ON l.merchant_id=o.merchant_id AND l.order_id=o.id
 WHERE p.merchant_id=s.merchant_id AND p.id=sqlc.arg(payment_id)::uuid
   AND p.customer_id=s.customer_id AND s.id IN (p.subscription_id,o.subscription_id,l.subscription_id)
)
ORDER BY s.id FOR UPDATE;

-- name: ListPaymentAccessGrants :many
-- Checkout lines bind the initial membership; subscription bills bind later
-- periods. Both retain accepted terms, without consulting today's price.
WITH periods AS (
SELECT p.id AS payment_id, p.merchant_id, p.customer_id,
       s.id AS subscription_id, l.product_id, l.quantity, o.subscription_bill_kind,
       coalesce(o.period_starts_at, s.started_at) AS starts_at,
       CASE WHEN l.provider_access_default THEN
         coalesce(o.period_ends_at, ps.first_due_at, s.started_at + l.billing_interval_hours * interval '1 hour')
       ELSE coalesce(o.period_starts_at, s.started_at) + l.access_duration_hours * interval '1 hour' END AS ends_at
FROM billing.payments p
JOIN billing.orders o ON o.merchant_id=p.merchant_id AND o.id=p.order_id AND o.customer_id=p.customer_id
JOIN billing.order_lines l ON l.merchant_id=o.merchant_id AND l.order_id=o.id
JOIN billing.subscriptions s ON s.merchant_id=p.merchant_id AND s.customer_id=p.customer_id
  AND s.id=coalesce(o.subscription_id,l.subscription_id)
LEFT JOIN billing.provider_schedules ps ON ps.merchant_id=p.merchant_id
  AND ps.subscription_id=s.id AND ps.initial_order_id=o.id
  AND ps.source_reference='order:' || o.id::text
WHERE p.merchant_id=sqlc.arg(merchant_id)::uuid AND p.id=sqlc.arg(payment_id)::uuid
)
SELECT g.* FROM billing.grants g
JOIN billing.payments p ON p.merchant_id=g.merchant_id AND p.customer_id=g.customer_id
WHERE p.merchant_id=sqlc.arg(merchant_id)::uuid AND p.id=sqlc.arg(payment_id)::uuid
  AND g.kind='access' AND g.event='grant'
  AND (g.payment_id=p.id OR EXISTS (
    SELECT 1 FROM periods w WHERE g.source_id=w.subscription_id::text AND g.product_id=w.product_id
      AND ((g.source_type='subscription' AND g.starts_at=w.starts_at AND g.ends_at IS NOT DISTINCT FROM w.ends_at)
        OR (g.source_type='grace' AND g.starts_at=w.ends_at))))
ORDER BY g.id;

-- name: RefundRevokedAccessWindow :one
-- Repair and ordinary grants honor the same retained access decision.
-- Resolve the customer's refunded windows before matching the requested product.
WITH periods AS MATERIALIZED (
SELECT p.id AS payment_id, p.merchant_id, p.customer_id,
       s.id AS subscription_id, l.product_id, l.quantity, o.subscription_bill_kind,
       coalesce(o.period_starts_at, s.started_at) AS starts_at,
       CASE WHEN l.provider_access_default THEN
         coalesce(o.period_ends_at, ps.first_due_at, s.started_at + l.billing_interval_hours * interval '1 hour')
       ELSE coalesce(o.period_starts_at, s.started_at) + l.access_duration_hours * interval '1 hour' END AS ends_at
FROM billing.payments p
JOIN billing.orders o ON o.merchant_id=p.merchant_id AND o.id=p.order_id AND o.customer_id=p.customer_id
JOIN billing.order_lines l ON l.merchant_id=o.merchant_id AND l.order_id=o.id
JOIN billing.subscriptions s ON s.merchant_id=p.merchant_id AND s.customer_id=p.customer_id
  AND s.id=coalesce(o.subscription_id,l.subscription_id)
LEFT JOIN billing.provider_schedules ps ON ps.merchant_id=p.merchant_id
  AND ps.subscription_id=s.id AND ps.initial_order_id=o.id
  AND ps.source_reference='order:' || o.id::text
WHERE p.merchant_id=sqlc.arg(merchant_id)::uuid AND p.customer_id=sqlc.arg(customer_id)::uuid
  AND p.metadata->>'refund_access_revoked'='true'
)
SELECT EXISTS (SELECT 1 WHERE EXISTS (
 SELECT 1 FROM billing.payments p
 WHERE p.merchant_id=sqlc.arg(merchant_id)::uuid AND p.customer_id=sqlc.arg(customer_id)::uuid
   AND p.id=sqlc.narg(payment_id)::uuid AND p.metadata->>'refund_access_revoked'='true'
) OR EXISTS (
 SELECT 1 FROM periods w
 WHERE w.customer_id=sqlc.arg(customer_id)::uuid AND w.subscription_id::text=sqlc.arg(source_id)::text
   AND w.product_id=sqlc.arg(product_id)::uuid
   AND ((sqlc.arg(source_type)::text='subscription' AND w.starts_at=sqlc.arg(starts_at)::timestamptz
     AND w.ends_at IS NOT DISTINCT FROM sqlc.narg(ends_at)::timestamptz)
     OR (sqlc.arg(source_type)::text='grace' AND w.ends_at=sqlc.arg(starts_at)::timestamptz))
));

-- name: RefundAccessNeedsReview :one
-- Adjustment bills and replacement grants may combine several payments.
-- Without their funding lineage, preserve access and raise a review finding.
WITH periods AS (
SELECT p.id AS payment_id, p.merchant_id, p.customer_id,
       s.id AS subscription_id, l.product_id, l.quantity, o.subscription_bill_kind,
       coalesce(o.period_starts_at, s.started_at) AS starts_at,
       CASE WHEN l.provider_access_default THEN
         coalesce(o.period_ends_at, ps.first_due_at, s.started_at + l.billing_interval_hours * interval '1 hour')
       ELSE coalesce(o.period_starts_at, s.started_at) + l.access_duration_hours * interval '1 hour' END AS ends_at
FROM billing.payments p
JOIN billing.orders o ON o.merchant_id=p.merchant_id AND o.id=p.order_id AND o.customer_id=p.customer_id
JOIN billing.order_lines l ON l.merchant_id=o.merchant_id AND l.order_id=o.id
JOIN billing.subscriptions s ON s.merchant_id=p.merchant_id AND s.customer_id=p.customer_id
  AND s.id=coalesce(o.subscription_id,l.subscription_id)
LEFT JOIN billing.provider_schedules ps ON ps.merchant_id=p.merchant_id
  AND ps.subscription_id=s.id AND ps.initial_order_id=o.id
  AND ps.source_reference='order:' || o.id::text
WHERE p.merchant_id=sqlc.arg(merchant_id)::uuid AND p.id=sqlc.arg(payment_id)::uuid
)
SELECT EXISTS (SELECT 1 WHERE NOT EXISTS (
 SELECT 1 FROM billing.payments p
 WHERE p.merchant_id=sqlc.arg(merchant_id)::uuid AND p.id=sqlc.arg(payment_id)::uuid
   AND p.metadata->>'refund_access_revoked'='true'
) AND (EXISTS (SELECT 1 FROM periods w WHERE w.subscription_bill_kind='adjustment')
 OR EXISTS (
 SELECT 1 FROM periods w JOIN billing.grants g ON g.merchant_id=w.merchant_id
   AND g.customer_id=w.customer_id AND g.source_id=w.subscription_id::text AND g.product_id=w.product_id
 WHERE g.source_type='subscription' AND g.kind='access' AND g.event='grant'
   AND NOT EXISTS (SELECT 1 FROM billing.grants t WHERE t.merchant_id=g.merchant_id AND t.supersedes_id=g.id)
   AND ((g.starts_at=w.starts_at AND (g.ends_at IS DISTINCT FROM w.ends_at OR coalesce(g.quantity, 1) <> coalesce(w.quantity, 1)))
     OR (g.starts_at>w.starts_at AND g.ends_at IS NOT DISTINCT FROM w.ends_at))
 ) OR EXISTS (
 SELECT 1 FROM billing.payments p WHERE p.merchant_id=sqlc.arg(merchant_id)::uuid
   AND p.id=sqlc.arg(payment_id)::uuid AND p.subscription_id IS NOT NULL AND NOT EXISTS (SELECT 1 FROM periods)
 )));
