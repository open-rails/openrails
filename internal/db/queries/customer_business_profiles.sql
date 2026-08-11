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

-- name: ListCustomerBusinessProfiles :many
-- Merchant's business roster, oldest onboarding first, keyset-free but capped:
-- business customers are operator-onboarded (tens, not millions), and the
-- or#910 cycle re-lists per pass anyway.
SELECT * FROM openrails.customer_business_profiles
WHERE merchant_id = $1
ORDER BY created_at, customer_id
LIMIT sqlc.arg(row_limit);
