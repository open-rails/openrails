-- name: UpsertMerchantConfiguration :exec
-- One merchant-scoped config row. Missing JSON keys are interpreted by service
-- defaults, not stored as extra rows.
INSERT INTO openrails.merchant_configurations (
    merchant_id, config, created_at, updated_at
) VALUES ($1, $2, $3, $4)
ON CONFLICT (merchant_id) DO UPDATE SET
    config = EXCLUDED.config,
    updated_at = EXCLUDED.updated_at;

-- name: GetMerchantConfiguration :one
SELECT * FROM openrails.merchant_configurations
WHERE merchant_id = $1
LIMIT 1;
