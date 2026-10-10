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
-- More than one captured charge for one subscription period (metadata
-- period_start). A charge without one is judged by cadence: two at one price
-- within half the shortest cycle (billing or trial); a price change starts new
-- coverage. Distinct periods are never a duplicate. Refunds net out both ways.
WITH charges AS (
    SELECT purch.id, purch.customer_id, purch.subscription_id, purch.price_id, purch.amount, purch.currency, purch.purchased_at,
           price.product_id, prod.key AS product_key,
           purch.metadata->>'period_start' AS period_start,
           LEAST(price.billing_interval_hours, COALESCE(price.trial_duration_hours, price.billing_interval_hours)) AS cycle_hours
    FROM billing.payments purch
    JOIN billing.prices price ON purch.price_id = price.id
    JOIN billing.products prod ON price.product_id = prod.id
    WHERE purch.merchant_id = sqlc.arg(merchant_id)::uuid AND price.merchant_id = sqlc.arg(merchant_id)::uuid AND prod.merchant_id = sqlc.arg(merchant_id)::uuid AND purch.deleted_at IS NULL
      AND purch.subscription_id IS NOT NULL
      AND purch.status = 'succeeded'
      AND purch.money_movement = 'rail'
      AND purch.amount > 0
      AND purch.refunded_payment_id IS NULL
      AND NOT EXISTS (
          SELECT 1 FROM billing.payments r WHERE r.merchant_id = sqlc.arg(merchant_id)::uuid AND r.refunded_payment_id = purch.id AND r.deleted_at IS NULL
      )
      AND (sqlc.narg(customer_id)::uuid IS NULL OR purch.customer_id = sqlc.narg(customer_id)::uuid)
),
unstamped AS (
    SELECT c.*, LAG(c.id) OVER w AS prev_id, LAG(c.purchased_at) OVER w AS prev_at
    FROM charges c
    WHERE c.period_start IS NULL
    WINDOW w AS (PARTITION BY c.subscription_id, c.price_id ORDER BY c.purchased_at, c.id)
),
groups AS (
    SELECT subscription_id, period_start AS period_key, id, customer_id, product_id, product_key, amount, currency, purchased_at
    FROM charges
    WHERE period_start IS NOT NULL
      AND (subscription_id, period_start) IN (
          SELECT subscription_id, period_start FROM charges WHERE period_start IS NOT NULL
          GROUP BY subscription_id, period_start HAVING COUNT(*) > 1)
    UNION ALL
    SELECT u.subscription_id, to_char(p.purchased_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'), x.id, x.customer_id, x.product_id, x.product_key, x.amount, x.currency, x.purchased_at
    FROM unstamped u
    JOIN charges p ON p.id = u.prev_id
    JOIN charges x ON x.id IN (u.id, u.prev_id)
    WHERE u.cycle_hours IS NOT NULL AND u.cycle_hours > 0
      AND u.purchased_at - u.prev_at < make_interval(secs => u.cycle_hours * 1800)
)
SELECT
    subscription_id,
    period_key::text AS period_key,
    MIN(customer_id::text)::text AS user_id,
    MIN(product_id::text)::uuid AS product_id,
    MIN(product_key)::text AS product_key,
    COUNT(*)::int AS count,
    ARRAY_AGG(id ORDER BY purchased_at DESC)::uuid[] AS payment_ids,
    SUM(amount)::bigint AS total_amount,
    MIN(currency)::text AS currency,
    MIN(purchased_at)::timestamptz AS first_date,
    MAX(purchased_at)::timestamptz AS last_date
FROM groups
GROUP BY subscription_id, period_key;

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
