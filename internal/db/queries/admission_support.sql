-- Admission-plane support tables: the or#897 billing-policy registry and its
-- bindings, plus hierarchical budget-scope policies (#473). The Postgres
-- rolling-budget engine (budget_inflight_holds / budget_window_state /
-- budget_reservations + FOR UPDATE) was removed in the #513 hard cut — budget
-- accounting now lives in the Redis spendgate.

-- name: UpsertBillingPolicy :exec
-- Declare (or redeclare) one named policy. The body is validated by the shared
-- normalizer before it gets here, so a stored policy is always an enforceable one.
INSERT INTO billing.billing_policies (
    id, merchant_id, name, policy, created_at, updated_at
) VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (merchant_id, name) DO UPDATE SET
    policy = EXCLUDED.policy,
    updated_at = EXCLUDED.updated_at;

-- name: ListBillingPolicies :many
-- Every named policy the merchant has declared, for the config-sync document.
SELECT * FROM billing.billing_policies
WHERE merchant_id = $1
ORDER BY name;

-- name: UpsertBillingPolicyBindingDefault :exec
-- The merchant-wide default rung: applies to every payer with no more specific
-- binding. ON CONFLICT targets the partial unique index for that rung.
INSERT INTO billing.billing_policy_bindings (
    id, merchant_id, customer_id, tier, policy_name, created_at, updated_at
) VALUES ($1, $2, NULL, NULL, $3, $4, $5)
ON CONFLICT (merchant_id) WHERE ((customer_id IS NULL) AND (tier IS NULL)) DO UPDATE SET
    policy_name = EXCLUDED.policy_name,
    updated_at = EXCLUDED.updated_at;

-- name: UpsertBillingPolicyBindingTier :exec
-- The per-tier rung: applies to every payer at one trust tier.
INSERT INTO billing.billing_policy_bindings (
    id, merchant_id, customer_id, tier, policy_name, created_at, updated_at
) VALUES ($1, $2, NULL, $3, $4, $5, $6)
ON CONFLICT (merchant_id, tier) WHERE ((customer_id IS NULL) AND (tier IS NOT NULL)) DO UPDATE SET
    policy_name = EXCLUDED.policy_name,
    updated_at = EXCLUDED.updated_at;

-- name: UpsertBillingPolicyBindingCustomer :exec
-- The per-customer rung: the merchant's runtime lever for one payer. Beats the
-- tier and default rungs.
INSERT INTO billing.billing_policy_bindings (
    id, merchant_id, customer_id, tier, policy_name, created_at, updated_at
) VALUES ($1, $2, $3, NULL, $4, $5, $6)
ON CONFLICT (merchant_id, customer_id) WHERE (customer_id IS NOT NULL) DO UPDATE SET
    policy_name = EXCLUDED.policy_name,
    updated_at = EXCLUDED.updated_at;

-- name: LockBillingPolicyName :one
SELECT name FROM billing.billing_policies
WHERE merchant_id = sqlc.arg(merchant_id) AND name = sqlc.arg(name)
FOR KEY SHARE;

-- name: DeleteCustomerBillingPolicyBinding :exec
DELETE FROM billing.billing_policy_bindings
WHERE merchant_id = sqlc.arg(merchant_id) AND customer_id = sqlc.arg(customer_id);

-- name: ListDeclarativeBillingPolicyBindings :many
-- The DECLARATIVE rungs (merchant default + per-tier) for the config-sync
-- document. Per-customer bindings are deliberately excluded: they are runtime
-- segmentation state whose row count follows customers, not configuration, so
-- enumerating them would scale with records on file — and dumping them would
-- put customer identifiers into a source-available manifest.
SELECT * FROM billing.billing_policy_bindings
WHERE merchant_id = $1 AND customer_id IS NULL
ORDER BY (tier IS NOT NULL) DESC, tier;

-- name: ResolveBillingPolicy :one
-- The effective policy for a (merchant, payer, tier): most specific rung wins —
-- the payer's own binding, else the tier's, else the merchant default. The FK
-- guarantees the joined policy exists, so a resolved binding always yields a body.
SELECT b.policy_name, p.policy
FROM billing.billing_policy_bindings b
JOIN billing.billing_policies p
  ON p.merchant_id = b.merchant_id AND p.name = b.policy_name
WHERE b.merchant_id = $1
  AND (b.customer_id = $2 OR b.customer_id IS NULL)
  AND (b.tier = $3 OR b.tier IS NULL)
ORDER BY (b.customer_id IS NOT NULL) DESC, (b.tier IS NOT NULL) DESC
LIMIT 1;

-- name: ListCustomerBillingPolicyAssignments :many
-- One binding per customer at most (the customer rung's unique index).
SELECT customer_id, policy_name FROM billing.billing_policy_bindings
WHERE merchant_id = sqlc.arg(merchant_id) AND customer_id = ANY(sqlc.arg(customer_ids)::uuid[])
LIMIT sqlc.arg(row_limit)::int;
