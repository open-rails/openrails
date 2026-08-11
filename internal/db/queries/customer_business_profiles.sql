-- openrails.customer_business_profiles: or#908 B2B onboarding record. Row
-- presence IS the business posture — see the table comment and
-- money/business_profile.go for the onboard/offboard chokepoints.

-- name: UpsertCustomerBusinessProfile :exec
INSERT INTO openrails.customer_business_profiles (
    merchant_id, customer_id, terms_version, terms_accepted_at,
    terms_accepted_by, kyc_reference, currency, budget_alert_thresholds,
    created_at, updated_at
) VALUES (
    $1, $2, sqlc.arg(terms_version), sqlc.arg(terms_accepted_at),
    sqlc.arg(terms_accepted_by), sqlc.arg(kyc_reference), sqlc.arg(currency),
    sqlc.arg(budget_alert_thresholds),
    sqlc.arg(now)::timestamptz, sqlc.arg(now)::timestamptz
)
ON CONFLICT (merchant_id, customer_id) DO UPDATE SET
    terms_version = EXCLUDED.terms_version,
    terms_accepted_at = EXCLUDED.terms_accepted_at,
    terms_accepted_by = EXCLUDED.terms_accepted_by,
    kyc_reference = EXCLUDED.kyc_reference,
    currency = EXCLUDED.currency,
    budget_alert_thresholds = EXCLUDED.budget_alert_thresholds,
    updated_at = EXCLUDED.updated_at;

-- name: GetCustomerBusinessProfile :one
SELECT * FROM openrails.customer_business_profiles
WHERE merchant_id = $1 AND customer_id = $2
LIMIT 1;

-- name: DeleteCustomerBusinessProfile :execrows
DELETE FROM openrails.customer_business_profiles
WHERE merchant_id = $1 AND customer_id = $2;

-- CROSS-MERCHANT: merchants with business-cycle work, through migration 0007's
-- SECURITY DEFINER work queue. Ids only (FC-16 R2).
-- name: ListBusinessCycleWorkMerchants :many
SELECT merchant_id FROM openrails.business_cycle_work_merchant_ids(
    sqlc.arg(merchant_limit)::int);

-- or#910 suspension-recommendation edges. Both are watermark CAS updates:
-- Recommend fires only while no episode is open, Clear only while one is —
-- racing evaluators collapse onto exactly one row change per direction.
-- name: RecommendBusinessSuspension :execrows
UPDATE openrails.customer_business_profiles
SET suspension_recommended_at = sqlc.arg(now)::timestamptz,
    suspension_reason = sqlc.arg(reason),
    updated_at = sqlc.arg(now)::timestamptz
WHERE merchant_id = $1 AND customer_id = $2
  AND suspension_recommended_at IS NULL;

-- name: ClearBusinessSuspensionRecommendation :execrows
UPDATE openrails.customer_business_profiles
SET suspension_recommended_at = NULL,
    suspension_reason = '',
    updated_at = sqlc.arg(now)::timestamptz
WHERE merchant_id = $1 AND customer_id = $2
  AND suspension_recommended_at IS NOT NULL;

-- name: ListCustomerBusinessProfiles :many
-- Merchant's business roster, oldest onboarding first, keyset-free but capped:
-- business customers are operator-onboarded (tens, not millions), and the
-- or#910 cycle re-lists per pass anyway.
SELECT * FROM openrails.customer_business_profiles
WHERE merchant_id = $1
ORDER BY created_at, customer_id
LIMIT sqlc.arg(row_limit);
