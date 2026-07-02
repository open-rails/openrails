-- Admission-plane support tables: per-(payer, tier) money policies and
-- hierarchical budget-scope policies (#473). The Postgres rolling-budget
-- engine (budget_inflight_holds / budget_window_state / budget_reservations
-- + FOR UPDATE) was removed in the #513 hard cut — budget accounting now
-- lives in the Redis spendgate.

-- name: UpsertPayerSpendLimit :exec
-- Per-(subject, tier) limit override. ON CONFLICT targets the partial unique
-- index for non-NULL subjects (#477).
INSERT INTO openrails.payer_spend_limits (
    id, merchant_id, customer_id, tier, policy, policy_version, created_at, updated_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
ON CONFLICT (merchant_id, customer_id, tier) WHERE (customer_id IS NOT NULL) DO UPDATE SET
    policy = EXCLUDED.policy,
    updated_at = EXCLUDED.updated_at;

-- name: UpsertPayerSpendLimitDefault :exec
-- Tenant-wide DEFAULT tier limit (#477): customer_id IS NULL applies to
-- every payer at this tier — the platform capacity ladder declared once. ON
-- CONFLICT targets the partial unique index for the NULL-subject default.
INSERT INTO openrails.payer_spend_limits (
    id, merchant_id, customer_id, tier, policy, policy_version, created_at, updated_at
) VALUES ($1, $2, NULL, $3, $4, $5, $6, $7)
ON CONFLICT (merchant_id, tier) WHERE (customer_id IS NULL) DO UPDATE SET
    policy = EXCLUDED.policy,
    updated_at = EXCLUDED.updated_at;

-- name: GetPayerSpendLimits :one
-- The effective limit for a (tenant, subject, tier): the subject's own override
-- if present, else the tenant-wide default (customer_id IS NULL, #477).
-- Subject-specific rows sort first so LIMIT 1 picks the override.
SELECT * FROM openrails.payer_spend_limits
WHERE merchant_id = $1 AND tier = $3
  AND (customer_id = $2 OR customer_id IS NULL)
ORDER BY (customer_id IS NOT NULL) DESC
LIMIT 1;

-- name: UpsertInvokerSpendLimit :exec
-- Per-invoker spend-limit upsert (#473/#517): the payer's cap on a delegated
-- invoker/role. Payer-set only (no owner discriminator).
INSERT INTO openrails.invoker_spend_limits (
    id, merchant_id, customer_id, scope, scope_key, windows, policy_version, created_at, updated_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
ON CONFLICT (merchant_id, customer_id, scope, scope_key) DO UPDATE SET
    windows = EXCLUDED.windows,
    updated_at = EXCLUDED.updated_at;

-- name: DeleteInvokerSpendLimit :execrows
DELETE FROM openrails.invoker_spend_limits
WHERE merchant_id = $1 AND customer_id = $2
  AND scope = $3 AND scope_key = $4;

-- name: ListInvokerSpendLimits :many
-- ALL invoker spend limits for a payer (the admit path reads every scope to
-- compose the verdict).
SELECT * FROM openrails.invoker_spend_limits
WHERE merchant_id = $1 AND customer_id = $2;

-- name: UpsertTierScheduleDefault :exec
-- Tenant-wide tier schedule upsert (#476): customer_id IS NULL is the
-- tenant's default ladder. owner is supplied by the caller's authz path.
INSERT INTO openrails.tier_schedules (
    id, merchant_id, customer_id, currency, owner, rungs, schedule_version, created_at, updated_at
) VALUES ($1, $2, NULL, sqlc.arg(currency), $3, $4, 1, $5, $6)
ON CONFLICT (merchant_id, owner, currency) WHERE (customer_id IS NULL) DO UPDATE SET
    rungs = EXCLUDED.rungs,
    schedule_version = openrails.tier_schedules.schedule_version + 1,
    updated_at = EXCLUDED.updated_at;

-- name: UpsertTierScheduleSubject :exec
-- Per-subject tier schedule override upsert (#476): takes precedence over the
-- tenant-wide default for that subject.
INSERT INTO openrails.tier_schedules (
    id, merchant_id, customer_id, currency, owner, rungs, schedule_version, created_at, updated_at
) VALUES ($1, $2, $3, sqlc.arg(currency), $4, $5, 1, $6, $7)
ON CONFLICT (merchant_id, customer_id, owner, currency) WHERE (customer_id IS NOT NULL) DO UPDATE SET
    rungs = EXCLUDED.rungs,
    schedule_version = openrails.tier_schedules.schedule_version + 1,
    updated_at = EXCLUDED.updated_at;

-- name: GetEffectiveTierSchedule :one
-- The effective schedule for a (tenant, subject): the subject's own override if
-- present, else the tenant-wide default (customer_id IS NULL). owner is
-- the read filter (auto-graduation reads owner='platform'). Subject-specific
-- rows sort first so LIMIT 1 picks the override.
SELECT * FROM openrails.tier_schedules
WHERE merchant_id = $1 AND owner = $2
  AND currency = sqlc.arg(currency)
  AND (customer_id = $3 OR customer_id IS NULL)
ORDER BY (customer_id IS NOT NULL) DESC
LIMIT 1;
