-- Admission-plane support tables: rolling money budgets (#304), the payment
-- blocklist (#300), and per-(payer, tier) throughput policies.

-- name: GetBudgetReservationByCoords :one
SELECT * FROM billing.budget_reservations
WHERE tenant_id = $1 AND tenant_subject_id = $2
  AND actor = $3 AND source = $4 AND source_id = $5
LIMIT 1;

-- name: InsertBudgetReservation :exec
INSERT INTO billing.budget_reservations (
    id, tenant_id, tenant_subject_id, actor,
    amount_micros, status, source, source_id, expires_at, created_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10);

-- name: CaptureBudgetReservation :execrows
UPDATE billing.budget_reservations
SET status = 'captured', captured_micros = $2
WHERE id = $1 AND status = 'active';

-- name: ReleaseBudgetReservation :execrows
UPDATE billing.budget_reservations
SET status = 'released'
WHERE id = $1 AND status = 'active';

-- name: AggregateBudgetWindow :one
-- One fixed window's state since window_start (#337): used = captured sums,
-- reserved = active sums.
SELECT COALESCE(SUM(captured_micros) FILTER (WHERE status = 'captured'), 0)::bigint AS used,
       COALESCE(SUM(amount_micros) FILTER (WHERE status = 'active'), 0)::bigint AS reserved
FROM billing.budget_reservations
WHERE tenant_id = $1 AND tenant_subject_id = $2 AND actor = $3
  AND created_at >= sqlc.arg(window_start)::timestamptz
  AND status IN ('active','captured');

-- name: GetBudgetWindowState :one
SELECT * FROM billing.budget_window_state bws
WHERE bws.tenant_id = $1 AND bws.tenant_subject_id = $2
  AND bws.actor = $3 AND bws.window_key = $4
LIMIT 1;

-- name: GetBudgetWindowStateForUpdate :one
-- The boundary-rollover serialization point: Reserve locks the state row so
-- concurrent reserves around a window boundary serialize on it (#337).
SELECT * FROM billing.budget_window_state bws
WHERE bws.tenant_id = $1 AND bws.tenant_subject_id = $2
  AND bws.actor = $3 AND bws.window_key = $4
FOR UPDATE;

-- name: InsertBudgetWindowStateIfAbsent :exec
-- First-ever charge on a window key: open at now, anchor at now.
INSERT INTO billing.budget_window_state (
    id, tenant_id, tenant_subject_id, actor, window_key,
    cadence, window_seconds, anchor, window_start, created_at, updated_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
ON CONFLICT (tenant_id, tenant_subject_id, actor, window_key) DO NOTHING;

-- name: ReopenBudgetWindowState :exec
-- Reopen an expired session window (opportunistically refreshing
-- cadence/window_seconds when the caller's policy changed).
UPDATE billing.budget_window_state
SET window_start = $2, cadence = $3, window_seconds = $4, updated_at = $5
WHERE id = $1;

-- name: InsertPaymentBlockIfAbsent :exec
INSERT INTO billing.payment_blocklist (
    id, tenant_id, tenant_subject_id, kind, value, reason, created_at
) VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (tenant_id, kind, value) DO NOTHING;

-- name: DeletePaymentBlock :exec
DELETE FROM billing.payment_blocklist
WHERE tenant_id = $1 AND kind = $2 AND value = $3;

-- name: PaymentBlockExists :one
SELECT EXISTS (
    SELECT 1 FROM billing.payment_blocklist
    WHERE tenant_id = $1 AND kind = $2 AND value = $3
);

-- name: UpsertTierPolicy :exec
INSERT INTO billing.tier_policies (
    id, tenant_id, tenant_subject_id, tier, policy, policy_version, created_at, updated_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
ON CONFLICT (tenant_id, tenant_subject_id, tier) DO UPDATE SET
    policy = EXCLUDED.policy,
    updated_at = EXCLUDED.updated_at;

-- name: GetTierPolicy :one
SELECT * FROM billing.tier_policies
WHERE tenant_id = $1 AND tenant_subject_id = $2 AND tier = $3
LIMIT 1;
