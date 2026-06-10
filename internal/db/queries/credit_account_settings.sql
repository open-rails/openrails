-- billing.credit_account_settings: per-(tenant, payer, credit_type) spend
-- policy + money-in state (#237/#239/#240/#241/#298/#299/#302).

-- name: GetCreditAccountSettings :one
SELECT * FROM billing.credit_account_settings
WHERE tenant_id = $1 AND tenant_subject_id = $2 AND credit_type_id = $3
LIMIT 1;

-- name: LockCreditAccountSettings :one
SELECT * FROM billing.credit_account_settings
WHERE tenant_id = $1 AND tenant_subject_id = $2 AND credit_type_id = $3
FOR UPDATE;

-- name: InsertCreditAccountSettingsIfAbsent :exec
-- Default settings row (hard-stop on, 80% alert threshold) in the given
-- billing mode; no-op when the row exists.
INSERT INTO billing.credit_account_settings (
    id, tenant_id, tenant_subject_id, credit_type_id, billing_mode,
    hard_stop_on_breach, alert_threshold_pct, created_at, updated_at
) VALUES ($1, $2, $3, $4, $5, true, 80, sqlc.arg(now), sqlc.arg(now))
ON CONFLICT (tenant_id, tenant_subject_id, credit_type_id) DO NOTHING;

-- name: UpsertCreditAccountSettings :exec
INSERT INTO billing.credit_account_settings (
    id, tenant_id, tenant_subject_id, credit_type_id, billing_mode,
    max_spend_per_day_micros, max_spend_per_month_micros, max_outstanding_owed_micros,
    low_balance_threshold_micros, auto_topup_enabled, auto_topup_amount_cents,
    auto_topup_payment_method_id, default_credit_expiry_days,
    hard_stop_on_breach, alert_threshold_pct, created_at, updated_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17)
ON CONFLICT (tenant_id, tenant_subject_id, credit_type_id) DO UPDATE SET
    billing_mode = EXCLUDED.billing_mode,
    max_spend_per_day_micros = EXCLUDED.max_spend_per_day_micros,
    max_spend_per_month_micros = EXCLUDED.max_spend_per_month_micros,
    max_outstanding_owed_micros = EXCLUDED.max_outstanding_owed_micros,
    low_balance_threshold_micros = EXCLUDED.low_balance_threshold_micros,
    auto_topup_enabled = EXCLUDED.auto_topup_enabled,
    auto_topup_amount_cents = EXCLUDED.auto_topup_amount_cents,
    auto_topup_payment_method_id = EXCLUDED.auto_topup_payment_method_id,
    default_credit_expiry_days = EXCLUDED.default_credit_expiry_days,
    hard_stop_on_breach = EXCLUDED.hard_stop_on_breach,
    alert_threshold_pct = EXCLUDED.alert_threshold_pct,
    updated_at = EXCLUDED.updated_at;

-- name: AddOutstandingOwed :exec
UPDATE billing.credit_account_settings
SET outstanding_owed_micros = outstanding_owed_micros + sqlc.arg(amount)::bigint,
    updated_at = sqlc.arg(now)
WHERE tenant_id = $1 AND tenant_subject_id = $2 AND credit_type_id = $3;

-- name: ReduceOutstandingOwedSnapshot :execrows
-- CAS-style decrement: only applies when owed still covers the snapshot, so
-- two collection runs racing on the same snapshot can't double-decrement.
UPDATE billing.credit_account_settings
SET outstanding_owed_micros = GREATEST(0, outstanding_owed_micros - sqlc.arg(snapshot)::bigint),
    updated_at = sqlc.arg(now)
WHERE tenant_id = $1 AND tenant_subject_id = $2 AND credit_type_id = $3
  AND outstanding_owed_micros >= sqlc.arg(snapshot)::bigint;

-- name: StampCreditAccountAlertAt :exec
UPDATE billing.credit_account_settings
SET last_alert_at = sqlc.arg(now), updated_at = sqlc.arg(now)
WHERE tenant_id = $1 AND tenant_subject_id = $2 AND credit_type_id = $3;

-- name: StampCreditAccountTopupAt :exec
UPDATE billing.credit_account_settings
SET last_topup_at = sqlc.arg(now), updated_at = sqlc.arg(now)
WHERE tenant_id = $1 AND tenant_subject_id = $2 AND credit_type_id = $3;

-- name: SetCreditAccountPaymentVerified :exec
UPDATE billing.credit_account_settings
SET verified_payment_method = sqlc.arg(verified)::boolean,
    verified_at = CASE WHEN sqlc.arg(verified)::boolean THEN sqlc.arg(now)::timestamptz ELSE NULL END,
    updated_at = sqlc.arg(now)::timestamptz
WHERE tenant_id = $1 AND tenant_subject_id = $2 AND credit_type_id = $3;

-- name: SuspendCreditAccount :exec
UPDATE billing.credit_account_settings
SET suspended_at = sqlc.arg(now)::timestamptz,
    suspend_reason = sqlc.narg(reason),
    updated_at = sqlc.arg(now)::timestamptz
WHERE tenant_id = $1 AND tenant_subject_id = $2 AND credit_type_id = $3;

-- name: ResumeCreditAccount :exec
UPDATE billing.credit_account_settings
SET suspended_at = NULL, suspend_reason = NULL, updated_at = $4
WHERE tenant_id = $1 AND tenant_subject_id = $2 AND credit_type_id = $3;

-- name: SetCreditAccountTier :exec
UPDATE billing.credit_account_settings
SET tier = sqlc.arg(tier)::text, updated_at = sqlc.arg(now)
WHERE tenant_id = $1 AND tenant_subject_id = $2 AND credit_type_id = $3;

-- name: ListBelowThresholdCreditAccounts :many
-- Money-in workers (#239/#240): accounts whose available balance
-- (balance - held) is under their configured low-balance threshold.
SELECT s.tenant_id, s.tenant_subject_id, s.credit_type_id,
       ct.name AS credit_type_name,
       (COALESCE(b.balance, 0) - COALESCE(b.held_balance, 0))::bigint AS available,
       COALESCE(s.low_balance_threshold_micros, 0)::bigint AS threshold,
       s.auto_topup_enabled, s.auto_topup_amount_cents, s.auto_topup_payment_method_id,
       s.last_alert_at, s.last_topup_at
FROM billing.credit_account_settings s
JOIN billing.credit_types ct ON ct.id = s.credit_type_id
LEFT JOIN billing.credit_balances b
  ON b.tenant_id = s.tenant_id
 AND b.tenant_subject_id = s.tenant_subject_id
 AND b.credit_type_id = s.credit_type_id
WHERE s.tenant_id = $1
  AND s.low_balance_threshold_micros IS NOT NULL
  AND (COALESCE(b.balance, 0) - COALESCE(b.held_balance, 0)) < s.low_balance_threshold_micros;

-- name: ListChargeableArrearsAccounts :many
-- Arrears collection (#241): accounts owing with a card on file.
-- min_threshold <= 0 means "charge every account with owed > 0".
SELECT s.tenant_id, s.tenant_subject_id, s.credit_type_id,
       ct.name AS credit_type_name,
       s.outstanding_owed_micros, s.auto_topup_payment_method_id
FROM billing.credit_account_settings s
JOIN billing.credit_types ct ON ct.id = s.credit_type_id
WHERE s.tenant_id = $1
  AND s.billing_mode = 'arrears'
  AND s.outstanding_owed_micros > 0
  AND s.auto_topup_payment_method_id IS NOT NULL
  AND (sqlc.arg(min_threshold)::bigint <= 0 OR s.outstanding_owed_micros >= sqlc.arg(min_threshold)::bigint);

-- name: GetCreditSpendLimit :one
SELECT * FROM billing.credit_spend_limits
WHERE tenant_id = $1 AND tenant_subject_id = $2 AND credit_type_id = $3 AND actor = $4
LIMIT 1;

-- name: UpsertCreditSpendLimit :exec
INSERT INTO billing.credit_spend_limits (
    id, tenant_id, tenant_subject_id, credit_type_id, actor,
    max_spend_per_day_micros, max_spend_per_month_micros, created_at, updated_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
ON CONFLICT (tenant_id, tenant_subject_id, credit_type_id, actor) DO UPDATE SET
    max_spend_per_day_micros = EXCLUDED.max_spend_per_day_micros,
    max_spend_per_month_micros = EXCLUDED.max_spend_per_month_micros,
    updated_at = EXCLUDED.updated_at;
