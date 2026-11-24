-- billing.dashboard_configs: per-merchant dashboard widget layout. An explicit
-- merchant_id scopes every statement to the request's merchant.

-- name: GetDashboardConfig :one
SELECT merchant_id, layout, updated_at, updated_by
FROM billing.dashboard_configs

WHERE dashboard_configs.merchant_id = sqlc.arg(merchant_id)::uuid
LIMIT 1;

-- name: UpsertDashboardConfig :one
INSERT INTO billing.dashboard_configs (merchant_id, layout, updated_by)
VALUES ($1, $2, $3)
ON CONFLICT (merchant_id)
DO UPDATE SET layout = EXCLUDED.layout, updated_by = EXCLUDED.updated_by, updated_at = now()
RETURNING merchant_id, layout, updated_at, updated_by;

-- name: GetDashboardProfile :one
-- What the merchant's data shows it sells: subscriptions, card payments,
-- metered usage and purchased credit lots. Decides the default dashboard.
SELECT EXISTS (SELECT 1 FROM billing.subscriptions WHERE subscriptions.merchant_id = sqlc.arg(merchant_id)::uuid)::boolean AS has_subscriptions,
    EXISTS (SELECT 1 FROM billing.payments WHERE payments.merchant_id = sqlc.arg(merchant_id)::uuid AND payments.rail IN ('nmi', 'ccbill', 'stripe'))::boolean AS has_card_payments,
    (EXISTS (SELECT 1 FROM billing.usage_unposted WHERE usage_unposted.merchant_id = sqlc.arg(merchant_id)::uuid)
     OR EXISTS (SELECT 1 FROM billing.usage_postings WHERE usage_postings.merchant_id = sqlc.arg(merchant_id)::uuid))::boolean AS has_usage,
    EXISTS (SELECT 1 FROM billing.credit_grants WHERE credit_grants.merchant_id = sqlc.arg(merchant_id)::uuid AND funding_source IN ('purchase', 'recurring'))::boolean AS has_credits;
