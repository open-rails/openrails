-- Admission-plane support tables: rolling money budgets (#304), the payment
-- blocklist (#300), and per-(payer, tier) throughput policies.

-- name: GetBudgetReservationByCoords :one
SELECT * FROM openrails.budget_inflight_holds
WHERE merchant_id = $1 AND customer_id = $2
  AND invoker_id = $3 AND currency = sqlc.arg(currency)
  AND source = $4 AND source_id = $5
LIMIT 1;

-- name: InsertBudgetReservation :exec
INSERT INTO openrails.budget_inflight_holds (
    id, merchant_id, customer_id, invoker_id, currency,
    amount, status, source, source_id, expires_at, created_at
) VALUES ($1, $2, $3, $4, sqlc.arg(currency), $5, $6, $7, $8, $9, $10);

-- name: CaptureBudgetReservation :execrows
UPDATE openrails.budget_inflight_holds
SET status = 'captured', captured = $2
WHERE id = $1 AND status = 'active';

-- name: ReleaseBudgetReservation :execrows
UPDATE openrails.budget_inflight_holds
SET status = 'released'
WHERE id = $1 AND status = 'active';

-- name: CaptureBudgetReservationsByCoords :execrows
-- Settle ALL scopes reserved under one request (#473): every active budget
-- reservation for (tenant, subject, source, source_id) -> captured. Idempotent
-- (already-settled rows are skipped by the status filter).
UPDATE openrails.budget_inflight_holds
SET status = 'captured', captured = sqlc.arg(captured)::bigint
WHERE merchant_id = $1 AND customer_id = $2
  AND currency = sqlc.arg(currency)
  AND source = $3 AND source_id = $4 AND status = 'active';

-- name: ReleaseBudgetReservationsByCoords :execrows
-- Free ALL scopes reserved under one request (#473): every active budget
-- reservation for (tenant, subject, source, source_id) -> released. Idempotent.
UPDATE openrails.budget_inflight_holds
SET status = 'released'
WHERE merchant_id = $1 AND customer_id = $2
  AND currency = sqlc.arg(currency)
  AND source = $3 AND source_id = $4 AND status = 'active';

-- name: AggregateBudgetWindow :one
-- One fixed window's state since window_start (#337): used = captured sums,
-- reserved = active sums.
SELECT COALESCE(SUM(captured) FILTER (WHERE status = 'captured'), 0)::bigint AS used,
       COALESCE(SUM(amount) FILTER (WHERE status = 'active'), 0)::bigint AS reserved
FROM openrails.budget_inflight_holds
WHERE merchant_id = $1 AND customer_id = $2 AND invoker_id = $3
  AND currency = sqlc.arg(currency)
  AND created_at >= sqlc.arg(window_start)::timestamptz
  AND status IN ('active','captured');

-- name: GetBudgetWindowState :one
SELECT * FROM openrails.budget_window_state bws
WHERE bws.merchant_id = $1 AND bws.customer_id = $2
  AND bws.invoker_id = $3 AND bws.currency = sqlc.arg(currency)
  AND bws.window_key = $4
LIMIT 1;

-- name: GetBudgetWindowStateForUpdate :one
-- The boundary-rollover serialization point: Reserve locks the state row so
-- concurrent reserves around a window boundary serialize on it (#337).
SELECT * FROM openrails.budget_window_state bws
WHERE bws.merchant_id = $1 AND bws.customer_id = $2
  AND bws.invoker_id = $3 AND bws.currency = sqlc.arg(currency)
  AND bws.window_key = $4
FOR UPDATE;

-- name: InsertBudgetWindowStateIfAbsent :exec
-- First-ever charge on a window key: open at now, anchor at now.
INSERT INTO openrails.budget_window_state (
    id, merchant_id, customer_id, invoker_id, currency, window_key,
    cadence, window_seconds, anchor, window_start, created_at, updated_at
) VALUES ($1, $2, $3, $4, sqlc.arg(currency), $5, $6, $7, $8, $9, $10, $11)
ON CONFLICT (merchant_id, customer_id, invoker_id, currency, window_key) DO NOTHING;

-- name: ReopenBudgetWindowState :exec
-- Reopen an expired session window (opportunistically refreshing
-- cadence/window_seconds when the caller's policy changed).
UPDATE openrails.budget_window_state
SET window_start = $2, cadence = $3, window_seconds = $4, updated_at = $5
WHERE id = $1;

-- name: InsertPaymentBlockIfAbsent :exec
INSERT INTO openrails.payment_blocklist (
    id, merchant_id, customer_id, kind, value, reason, created_at
) VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (merchant_id, kind, value) DO NOTHING;

-- name: DeletePaymentBlock :exec
DELETE FROM openrails.payment_blocklist
WHERE merchant_id = $1 AND kind = $2 AND value = $3;

-- name: PaymentBlockExists :one
SELECT EXISTS (
    SELECT 1 FROM openrails.payment_blocklist
    WHERE merchant_id = $1 AND kind = $2 AND value = $3
);

-- name: UpsertTierPolicy :exec
-- Per-(subject, tier) policy override. ON CONFLICT targets the partial unique
-- index for non-NULL subjects (#477).
INSERT INTO openrails.tier_policies (
    id, merchant_id, customer_id, tier, policy, policy_version, created_at, updated_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
ON CONFLICT (merchant_id, customer_id, tier) WHERE (customer_id IS NOT NULL) DO UPDATE SET
    policy = EXCLUDED.policy,
    updated_at = EXCLUDED.updated_at;

-- name: UpsertTierPolicyDefault :exec
-- Tenant-wide DEFAULT tier policy (#477): customer_id IS NULL applies to
-- every payer at this tier — the platform capacity ladder declared once. ON
-- CONFLICT targets the partial unique index for the NULL-subject default.
INSERT INTO openrails.tier_policies (
    id, merchant_id, customer_id, tier, policy, policy_version, created_at, updated_at
) VALUES ($1, $2, NULL, $3, $4, $5, $6, $7)
ON CONFLICT (merchant_id, tier) WHERE (customer_id IS NULL) DO UPDATE SET
    policy = EXCLUDED.policy,
    updated_at = EXCLUDED.updated_at;

-- name: GetTierPolicy :one
-- The effective policy for a (tenant, subject, tier): the subject's own override
-- if present, else the tenant-wide default (customer_id IS NULL, #477).
-- Subject-specific rows sort first so LIMIT 1 picks the override.
SELECT * FROM openrails.tier_policies
WHERE merchant_id = $1 AND tier = $3
  AND (customer_id = $2 OR customer_id IS NULL)
ORDER BY (customer_id IS NOT NULL) DESC
LIMIT 1;

-- name: UpsertBudgetPolicy :exec
-- Hierarchical money-budget policy upsert (#473). The owner discriminator is the
-- write-authz split (platform vs subject); the caller (service layer) enforces
-- that a subject path may only write owner='subject' rows.
INSERT INTO openrails.budget_policies (
    id, merchant_id, customer_id, scope, owner, scope_key, windows, policy_version, created_at, updated_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
ON CONFLICT (merchant_id, customer_id, scope, owner, scope_key) DO UPDATE SET
    windows = EXCLUDED.windows,
    updated_at = EXCLUDED.updated_at;

-- name: DeleteBudgetPolicy :execrows
DELETE FROM openrails.budget_policies
WHERE merchant_id = $1 AND customer_id = $2
  AND scope = $3 AND owner = $4 AND scope_key = $5;

-- name: ListBudgetPolicies :many
-- ALL budget policies for a subject regardless of owner (the admit path reads
-- every scope to compose the verdict).
SELECT * FROM openrails.budget_policies
WHERE merchant_id = $1 AND customer_id = $2;

-- name: ListBudgetPoliciesByOwner :many
-- Budget policies for a subject filtered by owner (the subject-facing read must
-- NOT expose platform-owned rows).
SELECT * FROM openrails.budget_policies
WHERE merchant_id = $1 AND customer_id = $2 AND owner = $3;

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
