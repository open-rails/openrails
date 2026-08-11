-- Admission-plane support tables: the or#897 billing-policy registry and its
-- bindings, plus hierarchical budget-scope policies (#473). The Postgres
-- rolling-budget engine (budget_inflight_holds / budget_window_state /
-- budget_reservations + FOR UPDATE) was removed in the #513 hard cut — budget
-- accounting now lives in the Redis spendgate.

-- name: UpsertBillingPolicy :exec
-- Declare (or redeclare) one named policy. The body is validated by the shared
-- normalizer before it gets here, so a stored policy is always an enforceable one.
INSERT INTO openrails.billing_policies (
    id, merchant_id, name, policy, created_at, updated_at
) VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (merchant_id, name) DO UPDATE SET
    policy = EXCLUDED.policy,
    updated_at = EXCLUDED.updated_at;

-- name: ListBillingPolicies :many
-- Every named policy the merchant has declared, for the config-sync document.
SELECT * FROM openrails.billing_policies
WHERE merchant_id = $1
ORDER BY name;

-- name: UpsertBillingPolicyBindingDefault :exec
-- The merchant-wide default rung: applies to every payer with no more specific
-- binding. ON CONFLICT targets the partial unique index for that rung.
INSERT INTO openrails.billing_policy_bindings (
    id, merchant_id, customer_id, tier, policy_name, created_at, updated_at
) VALUES ($1, $2, NULL, NULL, $3, $4, $5)
ON CONFLICT (merchant_id) WHERE ((customer_id IS NULL) AND (tier IS NULL)) DO UPDATE SET
    policy_name = EXCLUDED.policy_name,
    updated_at = EXCLUDED.updated_at;

-- name: UpsertBillingPolicyBindingTier :exec
-- The per-tier rung: applies to every payer at one trust tier.
INSERT INTO openrails.billing_policy_bindings (
    id, merchant_id, customer_id, tier, policy_name, created_at, updated_at
) VALUES ($1, $2, NULL, $3, $4, $5, $6)
ON CONFLICT (merchant_id, tier) WHERE ((customer_id IS NULL) AND (tier IS NOT NULL)) DO UPDATE SET
    policy_name = EXCLUDED.policy_name,
    updated_at = EXCLUDED.updated_at;

-- name: UpsertBillingPolicyBindingCustomer :exec
-- The per-customer rung: the merchant's runtime lever for one payer. Beats the
-- tier and default rungs.
INSERT INTO openrails.billing_policy_bindings (
    id, merchant_id, customer_id, tier, policy_name, created_at, updated_at
) VALUES ($1, $2, $3, NULL, $4, $5, $6)
ON CONFLICT (merchant_id, customer_id) WHERE (customer_id IS NOT NULL) DO UPDATE SET
    policy_name = EXCLUDED.policy_name,
    updated_at = EXCLUDED.updated_at;

-- name: ListDeclarativeBillingPolicyBindings :many
-- The DECLARATIVE rungs (merchant default + per-tier) for the config-sync
-- document. Per-customer bindings are deliberately excluded: they are runtime
-- segmentation state whose row count follows customers, not configuration, so
-- enumerating them would scale with records on file — and dumping them would
-- put customer identifiers into a source-available manifest.
SELECT * FROM openrails.billing_policy_bindings
WHERE merchant_id = $1 AND customer_id IS NULL
ORDER BY (tier IS NOT NULL) DESC, tier;

-- name: ResolveBillingPolicy :one
-- The effective policy for a (merchant, payer, tier): most specific rung wins —
-- the payer's own binding, else the tier's, else the merchant default. The FK
-- guarantees the joined policy exists, so a resolved binding always yields a body.
SELECT b.policy_name, p.policy
FROM openrails.billing_policy_bindings b
JOIN openrails.billing_policies p
  ON p.merchant_id = b.merchant_id AND p.name = b.policy_name
WHERE b.merchant_id = $1
  AND (b.customer_id = $2 OR b.customer_id IS NULL)
  AND (b.tier = $3 OR b.tier IS NULL)
ORDER BY (b.customer_id IS NOT NULL) DESC, (b.tier IS NOT NULL) DESC
LIMIT 1;

-- name: UpsertInvokerSpendLimit :exec
-- Per-invoker spend-limit upsert (#473/#517): the payer's cap on a delegated
-- invoker/role. Payer-set only (no owner discriminator). provenance (or#911)
-- is the caller's opaque reference for what authorized the grant; an upsert
-- replaces the whole grant, provenance included.
INSERT INTO openrails.invoker_spend_limits (
    id, merchant_id, customer_id, scope, scope_key, windows, provenance, created_at, updated_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
ON CONFLICT (merchant_id, customer_id, scope, scope_key) DO UPDATE SET
    windows = EXCLUDED.windows,
    provenance = EXCLUDED.provenance,
    updated_at = EXCLUDED.updated_at;

-- name: DeleteInvokerSpendLimit :execrows
-- Single-grant revocation (or#911): removes exactly one addressed delegation
-- and leaves every sibling untouched. 0 rows is a real answer (nothing at that
-- key), surfaced to the caller rather than swallowed.
DELETE FROM openrails.invoker_spend_limits
WHERE merchant_id = $1 AND customer_id = $2 AND scope = $3 AND scope_key = $4;

-- name: DeleteAllInvokerSpendLimits :execrows
-- Full-document replacement removes the exact merchant+payer set before
-- inserting the canonical replacement. This also purges legacy non-canonical
-- scope_key values that cannot be addressed safely by normalized key deletes.
DELETE FROM openrails.invoker_spend_limits
WHERE merchant_id = $1 AND customer_id = $2;

-- name: ListInvokerSpendLimits :many
-- ALL invoker spend limits for a payer (the admit path reads every scope to
-- compose the verdict).
SELECT * FROM openrails.invoker_spend_limits
WHERE merchant_id = $1 AND customer_id = $2;

-- name: UpsertTierScheduleDefault :exec
-- Tenant-wide tier schedule upsert (#476): customer_id IS NULL is the
-- tenant's default ladder. Schedules are platform-owned.
INSERT INTO openrails.tier_schedules (
    id, merchant_id, customer_id, currency, rungs, created_at, updated_at
) VALUES ($1, $2, NULL, sqlc.arg(currency), $3, $4, $5)
ON CONFLICT (merchant_id, currency) WHERE (customer_id IS NULL) DO UPDATE SET
    rungs = EXCLUDED.rungs,
    updated_at = EXCLUDED.updated_at;

-- name: UpsertTierScheduleSubject :exec
-- Per-subject tier schedule override upsert (#476): takes precedence over the
-- tenant-wide default for that subject.
INSERT INTO openrails.tier_schedules (
    id, merchant_id, customer_id, currency, rungs, created_at, updated_at
) VALUES ($1, $2, $3, sqlc.arg(currency), $4, $5, $6)
ON CONFLICT (merchant_id, customer_id, currency) WHERE (customer_id IS NOT NULL) DO UPDATE SET
    rungs = EXCLUDED.rungs,
    updated_at = EXCLUDED.updated_at;

-- name: GetEffectiveTierSchedule :one
-- The effective schedule for a (tenant, subject): the subject's own override if
-- present, else the tenant-wide default (customer_id IS NULL). Subject-specific
-- rows sort first so LIMIT 1 picks the override.
SELECT * FROM openrails.tier_schedules
WHERE merchant_id = $1
  AND currency = sqlc.arg(currency)
  AND (customer_id = $2 OR customer_id IS NULL)
ORDER BY (customer_id IS NOT NULL) DESC
LIMIT 1;
