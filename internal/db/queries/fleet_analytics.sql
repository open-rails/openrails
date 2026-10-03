-- Fleet dashboard: cross-merchant AGGREGATES only, through the SECURITY DEFINER
-- readers (or#861). A NULL exclude_merchant_id excludes nothing.

-- name: FleetMerchantFunnel :one
SELECT total::bigint AS total, armed::bigint AS armed, first_revenue::bigint AS first_revenue, active_revenue::bigint AS active_revenue
FROM billing.fleet_merchant_funnel(sqlc.narg(exclude_merchant_id)::uuid, sqlc.arg(since)::timestamptz);

-- name: FleetRevenueByCurrency :many
SELECT currency::text AS currency, payments::bigint AS payments, settled_amount::bigint AS settled_amount
FROM billing.fleet_revenue_by_currency(sqlc.narg(exclude_merchant_id)::uuid, sqlc.arg(since)::timestamptz);

-- name: FleetRailHealth :many
SELECT rail::text AS rail, succeeded::bigint AS succeeded, failed::bigint AS failed, chargebacks::bigint AS chargebacks
FROM billing.fleet_rail_health(sqlc.narg(exclude_merchant_id)::uuid, sqlc.arg(since)::timestamptz);

-- name: FleetMRRByCurrency :many
SELECT currency::text AS currency, subscriptions::bigint AS subscriptions, monthly_amount::bigint AS monthly_amount
FROM billing.fleet_mrr_by_currency(sqlc.narg(exclude_merchant_id)::uuid);

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

-- name: FleetWeeklyActiveMerchants :many
SELECT week_start::timestamptz AS week_start, merchants::bigint AS merchants
FROM billing.fleet_weekly_active_merchants(sqlc.narg(exclude_merchant_id)::uuid, sqlc.arg(since)::timestamptz);

-- name: FleetWeeklyCancelledSubscriptions :many
SELECT week_start::timestamptz AS week_start, cancellations::bigint AS cancellations
FROM billing.fleet_weekly_cancelled_subscriptions(sqlc.narg(exclude_merchant_id)::uuid, sqlc.arg(since)::timestamptz);

-- name: FleetWeeklyVolume :many
SELECT week_start::timestamptz AS week_start, currency::text AS currency, payments::bigint AS payments, settled_amount::bigint AS settled_amount
FROM billing.fleet_weekly_volume(sqlc.narg(exclude_merchant_id)::uuid, sqlc.arg(since)::timestamptz);
