-- CON plane (conPass) queries, all merchant-scoped. A non-NULL customer_id
-- restricts the scan to that customer (inline Converge); NULL scans the merchant.

-- A live window with a dangling subscription source is derive.access.unjustified's;
-- this reports only non-live dangling references (revoked/expired rows).
-- name: ConOrphanAccessSubscriptionSource :many
SELECT pa.id AS access_id, pa.customer_id::text AS user_id, pa.product_id, pa.source_type, pa.source_id
FROM billing.product_access pa
LEFT JOIN billing.subscriptions sub ON sub.merchant_id = sqlc.arg(merchant_id)::uuid AND pa.source_id = sub.id::text AND sub.deleted_at IS NULL
WHERE pa.merchant_id = sqlc.arg(merchant_id)::uuid AND pa.source_type = 'subscription'
  AND pa.deleted_at IS NULL
  AND sub.id IS NULL
  AND NOT (pa.revoked_at IS NULL
           AND pa.starts_at <= sqlc.arg(now)::timestamptz
           AND (pa.ends_at IS NULL OR pa.ends_at > sqlc.arg(now)::timestamptz))
  AND (sqlc.narg(customer_id)::uuid IS NULL OR pa.customer_id = sqlc.narg(customer_id)::uuid);

-- name: ConOrphanAccessPaymentSource :many
SELECT pa.id AS access_id, pa.customer_id::text AS user_id, pa.product_id, pa.source_type, pa.source_id
FROM billing.product_access pa
LEFT JOIN billing.payments purch ON purch.merchant_id = sqlc.arg(merchant_id)::uuid AND purch.id = pa.payment_id AND purch.deleted_at IS NULL
WHERE pa.merchant_id = sqlc.arg(merchant_id)::uuid AND pa.source_type = 'purchase'
  AND pa.deleted_at IS NULL
  AND purch.id IS NULL
  AND (sqlc.narg(customer_id)::uuid IS NULL OR pa.customer_id = sqlc.narg(customer_id)::uuid);


-- consistency.duplicate.ownership: more than one live purchased access grant for
-- one (customer, product), which the per-period charge check cannot see.
-- Bundle-included grants (source_id 'include:%') are excluded; a grant whose
-- payment is refunded (status, or a linked refund row) drops out. Purchases are
-- a jsonb array, oldest first: the last is the default cancel/refund target.
-- name: ConDuplicateOwnershipGrants :many
WITH live_ownership AS (
    SELECT g.id, g.customer_id, g.product_id, g.source_type, g.source_id,
           g.payment_id, g.starts_at, g.created_at,
           pay.amount AS payment_amount, pay.currency AS payment_currency,
           pay.purchased_at
    FROM billing.grants g
    LEFT JOIN billing.payments pay ON pay.merchant_id = sqlc.arg(merchant_id)::uuid AND pay.id = g.payment_id AND pay.deleted_at IS NULL
    WHERE g.merchant_id = sqlc.arg(merchant_id)::uuid AND g.event = 'grant' AND g.kind = 'access'
      AND g.source_type = 'purchase'
      AND (g.source_id IS NULL OR g.source_id NOT LIKE 'include:%')
      AND g.starts_at <= sqlc.arg(now)::timestamptz
      AND (g.ends_at IS NULL OR g.ends_at > sqlc.arg(now)::timestamptz)
      AND (sqlc.narg(customer_id)::uuid IS NULL OR g.customer_id = sqlc.narg(customer_id)::uuid)
      AND NOT EXISTS (
          SELECT 1 FROM billing.grants t
          WHERE t.merchant_id = sqlc.arg(merchant_id)::uuid AND t.supersedes_id = g.id AND t.event IN ('revoke', 'expire', 'supersede')
      )
      AND (pay.id IS NULL OR (pay.status <> 'refunded' AND NOT EXISTS (
          SELECT 1 FROM billing.payments r WHERE r.merchant_id = sqlc.arg(merchant_id)::uuid AND r.refunded_payment_id = pay.id AND r.deleted_at IS NULL
      )))
)
SELECT lo.customer_id, lo.product_id, prod.key AS product_key,
       COUNT(*)::int AS count,
       jsonb_agg(jsonb_build_object(
           'grant_id', lo.id,
           'source_type', lo.source_type,
           'source_id', lo.source_id,
           'payment_id', lo.payment_id,
           'amount', lo.payment_amount,
           'currency', lo.payment_currency,
           'purchased_at', COALESCE(lo.purchased_at, lo.starts_at)
       ) ORDER BY COALESCE(lo.purchased_at, lo.starts_at), lo.created_at) AS purchases
FROM live_ownership lo
JOIN billing.products prod ON prod.id = lo.product_id

WHERE prod.merchant_id = sqlc.arg(merchant_id)::uuid
GROUP BY lo.customer_id, lo.product_id, prod.key
HAVING COUNT(*) > 1;

-- name: ConDuplicateChargesSamePeriod :many
-- A subscription order may have multiple genuine partial payments. Report
-- excess net captured money against its frozen total, never infer duplicate
-- periods from current price cadence or receipt-time proximity.
WITH charges AS (
    SELECT p.id, p.customer_id, p.subscription_id, p.order_id, p.amount, p.currency, p.purchased_at,
           o.total,
           p.amount + COALESCE((SELECT SUM(r.amount) FROM billing.payments r
             WHERE r.merchant_id=p.merchant_id AND r.refunded_payment_id=p.id
               AND r.status='succeeded' AND r.deleted_at IS NULL),0) AS net_amount
    FROM billing.payments p
    JOIN billing.orders o ON o.merchant_id=p.merchant_id AND o.id=p.order_id
    WHERE p.merchant_id=sqlc.arg(merchant_id)::uuid AND p.deleted_at IS NULL
      AND p.subscription_id IS NOT NULL AND p.status IN ('succeeded','refunded')
      AND p.money_movement='rail' AND p.amount>0 AND p.refunded_payment_id IS NULL
      AND (sqlc.narg(customer_id)::uuid IS NULL OR p.customer_id=sqlc.narg(customer_id)::uuid)
), excess AS (
    SELECT subscription_id, order_id, MIN(customer_id::text)::text AS user_id,
           COUNT(*)::int AS count, ARRAY_AGG(id ORDER BY purchased_at DESC)::uuid[] AS payment_ids,
           SUM(net_amount)::bigint AS total_amount, MIN(currency)::text AS currency,
           MIN(purchased_at)::timestamptz AS first_date, MAX(purchased_at)::timestamptz AS last_date
    FROM charges GROUP BY subscription_id, order_id HAVING SUM(net_amount)>MAX(total)
)
SELECT e.subscription_id, e.order_id::text AS period_key, e.user_id,
       l.product_id, l.product_key, e.count, e.payment_ids, e.total_amount, e.currency, e.first_date, e.last_date
FROM excess e
JOIN LATERAL (SELECT line.product_id, prod.key::text AS product_key
    FROM billing.order_lines line JOIN billing.products prod ON prod.merchant_id=line.merchant_id AND prod.id=line.product_id
    WHERE line.merchant_id=sqlc.arg(merchant_id)::uuid AND line.order_id=e.order_id
    ORDER BY line.position LIMIT 1) l ON true;

-- name: ResolveVanishedFindings :execrows
-- Open findings of one type under a subject prefix that the latest full scan
-- no longer reports close themselves.
UPDATE billing.reconciliation_findings
   SET status = 'fixed', resolution = 'auto_vanished', resolved_at = now()
 WHERE merchant_id = sqlc.arg(merchant_id)::uuid
   AND finding_type = sqlc.arg(finding_type)::text
   AND subject_key LIKE sqlc.arg(subject_prefix)::text || '%'
   AND NOT (subject_key = ANY(sqlc.arg(keep_subjects)::text[]))
   AND status IN ('reconcile_required', 'requires_review');
