-- Fleet dashboard: cross-merchant aggregates only, never merchant rows. A NULL
-- exclude_merchant_id excludes nothing.

-- name: FleetMerchantFunnel :one
SELECT count(*)::bigint AS total,
       (count(*) FILTER (WHERE EXISTS (
           SELECT 1 FROM billing.psps p
            WHERE p.merchant_id = m.id AND p.superseded_at IS NULL)))::bigint AS armed,
       (count(*) FILTER (WHERE EXISTS (
           SELECT 1 FROM billing.payments pay
            WHERE pay.merchant_id = m.id AND pay.status = 'succeeded'
              AND pay.reversal_kind IS NULL AND pay.deleted_at IS NULL)))::bigint AS first_revenue,
       (count(*) FILTER (WHERE EXISTS (
           SELECT 1 FROM billing.payments pay
            WHERE pay.merchant_id = m.id AND pay.status = 'succeeded'
              AND pay.reversal_kind IS NULL AND pay.deleted_at IS NULL
              AND pay.purchased_at >= sqlc.arg(since)::timestamptz)))::bigint AS active_revenue
FROM billing.merchants m
WHERE m.deleted_at IS NULL AND m.status = 'active'
  AND (sqlc.narg(exclude_merchant_id)::uuid IS NULL OR m.id <> sqlc.narg(exclude_merchant_id)::uuid);

-- Settled sale volume per currency in the window. Sale rows only: reversal
-- mirror rows share status=completed and must never count.
-- name: FleetRevenueByCurrency :many
SELECT p.currency::text AS currency, count(*)::bigint AS payments, COALESCE(sum(p.amount), 0)::bigint AS settled_amount
FROM billing.payments p
WHERE p.status = 'succeeded' AND p.reversal_kind IS NULL AND p.deleted_at IS NULL
  AND p.purchased_at >= sqlc.arg(since)::timestamptz
  AND (sqlc.narg(exclude_merchant_id)::uuid IS NULL OR p.merchant_id <> sqlc.narg(exclude_merchant_id)::uuid)
GROUP BY p.currency
ORDER BY p.currency;

-- Per-rail approved and declined charge attempts and chargebacks in the window.
-- name: FleetRailHealth :many
WITH charges AS (
    SELECT a.rail AS r,
           count(*) FILTER (WHERE a.category = 'approved') AS ok,
           count(*) FILTER (WHERE a.category <> 'approved') AS refused
      FROM billing.payment_attempts a
     WHERE a.attempted_at >= sqlc.arg(since)::timestamptz AND a.kind <> 'verify'
       AND (sqlc.narg(exclude_merchant_id)::uuid IS NULL OR a.merchant_id <> sqlc.narg(exclude_merchant_id)::uuid)
     GROUP BY a.rail
), disputes AS (
    SELECT p.rail AS r, count(*) AS n
      FROM billing.payments p
     WHERE p.purchased_at >= sqlc.arg(since)::timestamptz AND p.reversal_kind = 'chargeback' AND p.status = 'succeeded'
       AND p.deleted_at IS NULL
       AND (sqlc.narg(exclude_merchant_id)::uuid IS NULL OR p.merchant_id <> sqlc.narg(exclude_merchant_id)::uuid)
     GROUP BY p.rail
)
SELECT COALESCE(c.r, d.r)::text AS rail, COALESCE(c.ok, 0)::bigint AS succeeded,
       COALESCE(c.refused, 0)::bigint AS failed, COALESCE(d.n, 0)::bigint AS chargebacks
FROM charges c
FULL JOIN disputes d ON d.r = c.r
ORDER BY 1;

-- Fleet MRR per currency, using the dashboard mrr normalization.
-- name: FleetMRRByCurrency :many
SELECT pr.currency::text AS currency, count(*)::bigint AS subscriptions,
       COALESCE(sum(billing.monthly_normalized_amount(pr.amount, pr.billing_interval_hours)), 0)::bigint AS monthly_amount
FROM billing.subscriptions s
JOIN billing.prices pr ON pr.merchant_id = s.merchant_id AND pr.id = s.price_id
WHERE s.status = 'active' AND s.deleted_at IS NULL AND pr.billing_interval_hours IS NOT NULL AND pr.billing_interval_hours > 0
  AND (sqlc.narg(exclude_merchant_id)::uuid IS NULL OR s.merchant_id <> sqlc.narg(exclude_merchant_id)::uuid)
GROUP BY pr.currency
ORDER BY pr.currency;

-- Canonical week list so bucket alignment matches the aggregates' date_trunc.
-- name: FleetWeeks :many
SELECT generate_series(date_trunc('week', sqlc.arg(since)::timestamptz), date_trunc('week', now()), interval '7 days')::timestamptz AS week_start;

-- name: FleetWeeklyNewMerchants :many
SELECT date_trunc('week', created_at)::timestamptz AS week_start, count(*)::bigint AS merchants
FROM billing.merchants
WHERE deleted_at IS NULL AND status = 'active'
  AND created_at >= date_trunc('week', sqlc.arg(since)::timestamptz)
  AND (sqlc.narg(exclude_merchant_id)::uuid IS NULL OR id <> sqlc.narg(exclude_merchant_id)::uuid)
GROUP BY 1;

-- Weekly count of distinct merchants with a settled sale; a count, never the list.
-- name: FleetWeeklyActiveMerchants :many
SELECT date_trunc('week', p.purchased_at)::timestamptz AS week_start, count(DISTINCT p.merchant_id)::bigint AS merchants
FROM billing.payments p
WHERE p.status = 'succeeded' AND p.reversal_kind IS NULL AND p.deleted_at IS NULL
  AND p.purchased_at >= date_trunc('week', sqlc.arg(since)::timestamptz)
  AND (sqlc.narg(exclude_merchant_id)::uuid IS NULL OR p.merchant_id <> sqlc.narg(exclude_merchant_id)::uuid)
GROUP BY 1;

-- Weekly subscription cancellations: the churn proxy on the fleet trend chart.
-- name: FleetWeeklyCanceledSubscriptions :many
SELECT date_trunc('week', s.canceled_at)::timestamptz AS week_start, count(*)::bigint AS cancellations
FROM billing.subscriptions s
WHERE s.canceled_at IS NOT NULL AND s.deleted_at IS NULL
  AND s.canceled_at >= date_trunc('week', sqlc.arg(since)::timestamptz)
  AND (sqlc.narg(exclude_merchant_id)::uuid IS NULL OR s.merchant_id <> sqlc.narg(exclude_merchant_id)::uuid)
GROUP BY 1;

-- Weekly settled sale volume per currency. Sale rows only.
-- name: FleetWeeklyVolume :many
SELECT date_trunc('week', p.purchased_at)::timestamptz AS week_start, p.currency::text AS currency,
       count(*)::bigint AS payments, COALESCE(sum(p.amount), 0)::bigint AS settled_amount
FROM billing.payments p
WHERE p.status = 'succeeded' AND p.reversal_kind IS NULL AND p.deleted_at IS NULL
  AND p.purchased_at >= date_trunc('week', sqlc.arg(since)::timestamptz)
  AND (sqlc.narg(exclude_merchant_id)::uuid IS NULL OR p.merchant_id <> sqlc.narg(exclude_merchant_id)::uuid)
GROUP BY 1, 2
ORDER BY 1, 2;
