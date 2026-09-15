-- name: LockMerchantSettings :one
SELECT id FROM openrails.merchants WHERE id = $1 FOR UPDATE;

-- name: ReadMerchantSettingsLock :one
SELECT id FROM openrails.merchants WHERE id = $1 FOR SHARE;

-- name: ListDefaultTrustLevelSchedules :many
SELECT currency, rungs FROM openrails.tier_schedules
WHERE merchant_id = $1 AND customer_id IS NULL
AND currency > sqlc.arg(after_currency)::text ORDER BY currency LIMIT 100;

-- name: DeleteDefaultTrustLevelSchedules :exec
DELETE FROM openrails.tier_schedules WHERE merchant_id = $1 AND customer_id IS NULL;

-- name: DeleteDeclarativeBillingPolicyBindings :exec
DELETE FROM openrails.billing_policy_bindings WHERE merchant_id = $1 AND customer_id IS NULL;

-- name: FindRemovedCustomerPolicies :many
SELECT DISTINCT policy_name FROM openrails.billing_policy_bindings
WHERE merchant_id = $1 AND customer_id IS NOT NULL
AND NOT (policy_name = ANY(COALESCE(sqlc.arg(names)::text[], '{}')))
ORDER BY policy_name LIMIT 1;

-- name: DeleteUndeclaredBillingPolicies :exec
DELETE FROM openrails.billing_policies WHERE merchant_id = $1
AND NOT (name = ANY(COALESCE(sqlc.arg(names)::text[], '{}')));
