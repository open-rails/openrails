-- name: UpsertMerchantConfiguration :exec
-- One merchant-scoped config row. Missing JSON keys are interpreted by service
-- defaults, not stored as extra rows.
INSERT INTO openrails.merchant_configurations (
    merchant_id, config, config_version, created_at, updated_at
) VALUES ($1, $2, 1, $3, $4)
ON CONFLICT (merchant_id) DO UPDATE SET
    config = EXCLUDED.config,
    config_version = openrails.merchant_configurations.config_version + 1,
    updated_at = EXCLUDED.updated_at;

-- name: GetMerchantConfiguration :one
SELECT * FROM openrails.merchant_configurations
WHERE merchant_id = $1
LIMIT 1;
