-- Admission-plane support: customers' own billing policies (the named policies,
-- tier map and default are the merchant's settings), plus hierarchical
-- budget-scope policies (#473). The Postgres
-- rolling-budget engine (budget_inflight_holds / budget_window_state /
-- budget_reservations + FOR UPDATE) was removed in the #513 hard cut — budget
-- accounting now lives in the Redis spendgate.

-- name: SetCustomerBillingPolicy :execrows
-- The customer's own billing policy, by its name in the merchant's settings:
-- staff's lever for one payer, beating the tier and the default. NULL clears it.
UPDATE billing.customers SET billing_policy = sqlc.narg(policy)::text
WHERE merchant_id = sqlc.arg(merchant_id) AND id = sqlc.arg(customer_id);

-- name: GetCustomerBillingPolicy :one
SELECT billing_policy FROM billing.customers
WHERE merchant_id = sqlc.arg(merchant_id) AND id = sqlc.arg(customer_id);

-- name: ListCustomerBillingPolicyAssignments :many
SELECT id AS customer_id, billing_policy::text AS policy_name FROM billing.customers
WHERE merchant_id = sqlc.arg(merchant_id) AND id = ANY(sqlc.arg(customer_ids)::uuid[]) AND billing_policy IS NOT NULL
LIMIT sqlc.arg(row_limit)::int;

-- name: FindAssignedBillingPolicyOutside :one
-- A billing policy some customer is assigned that names lacks: a settings
-- change may not remove it.
SELECT billing_policy::text FROM billing.customers
WHERE merchant_id = sqlc.arg(merchant_id) AND billing_policy IS NOT NULL
  AND NOT (billing_policy = ANY(COALESCE(sqlc.arg(names)::text[], '{}')))
LIMIT 1;
