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
    amount_millicents, status, source, source_id, expires_at, created_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10);

-- name: CaptureBudgetReservation :execrows
UPDATE billing.budget_reservations
SET status = 'captured', captured_millicents = $2
WHERE id = $1 AND status = 'active';

-- name: ReleaseBudgetReservation :execrows
UPDATE billing.budget_reservations
SET status = 'released'
WHERE id = $1 AND status = 'active';

-- name: AggregateBudgetWindow :one
-- One rolling window's state: used = captured sums, reserved = active sums,
-- oldest = the in-window reservation whose age-out frees the window.
-- oldest uses the zero-time sentinel for "no in-window reservations" (sqlc
-- infers aggregate outputs non-nullable; callers check IsZero).
SELECT COALESCE(SUM(captured_millicents) FILTER (WHERE status = 'captured'), 0)::bigint AS used,
       COALESCE(SUM(amount_millicents) FILTER (WHERE status = 'active'), 0)::bigint AS reserved,
       COALESCE(MIN(created_at) FILTER (WHERE status IN ('active','captured')),
                '0001-01-01 00:00:00+00'::timestamptz) AS oldest
FROM billing.budget_reservations
WHERE tenant_id = $1 AND tenant_subject_id = $2 AND actor = $3
  AND created_at >= sqlc.arg(window_start)::timestamptz
  AND status IN ('active','captured');

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
