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
-- metered usage, purchased credit lots and checkouts. Decides the default
-- dashboard.
SELECT EXISTS (SELECT 1 FROM billing.subscriptions WHERE subscriptions.merchant_id = sqlc.arg(merchant_id)::uuid)::boolean AS has_subscriptions,
    EXISTS (SELECT 1 FROM billing.payments WHERE payments.merchant_id = sqlc.arg(merchant_id)::uuid AND payments.rail IN ('nmi', 'ccbill', 'stripe'))::boolean AS has_card_payments,
    EXISTS (SELECT 1 FROM billing.usage_daily WHERE usage_daily.merchant_id = sqlc.arg(merchant_id)::uuid)::boolean AS has_usage,
    EXISTS (SELECT 1 FROM billing.grants WHERE grants.merchant_id = sqlc.arg(merchant_id)::uuid AND kind = 'credit' AND event = 'grant' AND source_type = 'purchase')::boolean AS has_credits,
    EXISTS (SELECT 1 FROM billing.orders WHERE orders.merchant_id = sqlc.arg(merchant_id)::uuid AND purchase_number IS NOT NULL)::boolean AS has_checkouts;
