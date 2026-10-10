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

-- name: HasUsageActivity :one
-- Any usage-stream signal for the merchant: metered events or purchased credit
-- lots. Decides whether the default dashboard seeds usage widgets.
SELECT (EXISTS (SELECT 1 FROM billing.usage_events
WHERE usage_events.merchant_id = sqlc.arg(merchant_id)::uuid
)
    OR EXISTS (SELECT 1 FROM billing.grants WHERE grants.merchant_id = sqlc.arg(merchant_id)::uuid AND kind = 'credit' AND event = 'grant' AND source_type = 'purchase'))::boolean AS has_activity;
