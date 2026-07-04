-- openrails.dashboard_configs — #741 per-merchant dashboard widget layout.
-- RLS (app.merchant_id GUC) scopes every statement to the request's merchant.

-- name: GetDashboardConfig :one
SELECT merchant_id, layout, updated_at, updated_by
FROM openrails.dashboard_configs
LIMIT 1;

-- name: UpsertDashboardConfig :one
INSERT INTO openrails.dashboard_configs (merchant_id, layout, updated_by)
VALUES ($1, $2, $3)
ON CONFLICT (merchant_id)
DO UPDATE SET layout = EXCLUDED.layout, updated_by = EXCLUDED.updated_by, updated_at = now()
RETURNING merchant_id, layout, updated_at, updated_by;

-- name: HasUsageActivity :one
-- Any usage-stream signal for the merchant: metered events or purchased credit
-- lots. Drives seeding usage widgets into the #741 default dashboard template.
SELECT (EXISTS (SELECT 1 FROM openrails.usage_events)
    OR EXISTS (SELECT 1 FROM openrails.grants WHERE kind = 'credit' AND event = 'grant' AND source_type = 'purchase'))::boolean AS has_activity;
