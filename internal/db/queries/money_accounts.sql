-- openrails.money_accounts: per-(tenant, payer, currency) spend policy + money-in
-- state (#237/#239/#240/#241/#298/#299/#302). amounts are the currency's minor unit
-- (default µ$). currency is a system code; the Go registry is authority.

-- name: GetMoneyAccountSettings :one
SELECT * FROM openrails.money_accounts
WHERE tenant_id = $1 AND tenant_subject_id = $2 AND currency = sqlc.arg(currency)
LIMIT 1;

-- name: LockMoneyAccountSettings :one
SELECT * FROM openrails.money_accounts
WHERE tenant_id = $1 AND tenant_subject_id = $2 AND currency = sqlc.arg(currency)
FOR UPDATE;

-- name: InsertMoneyAccountSettingsIfAbsent :exec
-- Default settings row (hard-stop on, 80% alert threshold) in the given
-- billing mode; no-op when the row exists.
INSERT INTO openrails.money_accounts (
    id, tenant_id, tenant_subject_id, currency, billing_mode,
    hard_stop_on_breach, alert_threshold_pct, created_at, updated_at
) VALUES ($1, $2, $3, sqlc.arg(currency), $4, true, 80, sqlc.arg(now), sqlc.arg(now))
ON CONFLICT (tenant_id, tenant_subject_id, currency) DO NOTHING;

-- name: UpsertMoneyAccountSettings :exec
INSERT INTO openrails.money_accounts (
    id, tenant_id, tenant_subject_id, currency, billing_mode,
    max_spend_per_day_micros, max_spend_per_month_micros, max_outstanding_owed_micros,
    low_balance_threshold_micros, auto_topup_enabled, auto_topup_amount_cents,
    auto_topup_payment_method_id, default_credit_expiry_days,
    hard_stop_on_breach, alert_threshold_pct, created_at, updated_at
) VALUES ($1, $2, $3, sqlc.arg(currency), $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)
ON CONFLICT (tenant_id, tenant_subject_id, currency) DO UPDATE SET
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

-- name: AddMoneyOutstandingOwed :exec
UPDATE openrails.money_accounts
SET outstanding_owed_micros = outstanding_owed_micros + sqlc.arg(amount)::bigint,
    updated_at = sqlc.arg(now)
WHERE tenant_id = $1 AND tenant_subject_id = $2 AND currency = sqlc.arg(currency);

-- name: ReduceMoneyOutstandingOwedSnapshot :execrows
-- CAS-style decrement: only applies when owed still covers the snapshot, so
-- two collection runs racing on the same snapshot can't double-decrement.
UPDATE openrails.money_accounts
SET outstanding_owed_micros = GREATEST(0, outstanding_owed_micros - sqlc.arg(snapshot)::bigint),
    updated_at = sqlc.arg(now)
WHERE tenant_id = $1 AND tenant_subject_id = $2 AND currency = sqlc.arg(currency)
  AND outstanding_owed_micros >= sqlc.arg(snapshot)::bigint;

-- name: StampMoneyAccountAlertAt :exec
UPDATE openrails.money_accounts
SET last_alert_at = sqlc.arg(now), updated_at = sqlc.arg(now)
WHERE tenant_id = $1 AND tenant_subject_id = $2 AND currency = sqlc.arg(currency);

-- name: StampMoneyAccountTopupAt :exec
UPDATE openrails.money_accounts
SET last_topup_at = sqlc.arg(now), updated_at = sqlc.arg(now)
WHERE tenant_id = $1 AND tenant_subject_id = $2 AND currency = sqlc.arg(currency);

-- name: SetMoneyAccountPaymentVerified :exec
UPDATE openrails.money_accounts
SET verified_payment_method = sqlc.arg(verified)::boolean,
    verified_at = CASE WHEN sqlc.arg(verified)::boolean THEN sqlc.arg(now)::timestamptz ELSE NULL END,
    updated_at = sqlc.arg(now)::timestamptz
WHERE tenant_id = $1 AND tenant_subject_id = $2 AND currency = sqlc.arg(currency);

-- name: SuspendMoneyAccount :exec
UPDATE openrails.money_accounts
SET suspended_at = sqlc.arg(now)::timestamptz,
    suspend_reason = sqlc.narg(reason),
    updated_at = sqlc.arg(now)::timestamptz
WHERE tenant_id = $1 AND tenant_subject_id = $2 AND currency = sqlc.arg(currency);

-- name: ResumeMoneyAccount :exec
UPDATE openrails.money_accounts
SET suspended_at = NULL, suspend_reason = NULL, updated_at = $3
WHERE tenant_id = $1 AND tenant_subject_id = $2 AND currency = sqlc.arg(currency);

-- name: SetMoneyAccountTier :exec
UPDATE openrails.money_accounts
SET tier = sqlc.arg(tier)::text, updated_at = sqlc.arg(now)
WHERE tenant_id = $1 AND tenant_subject_id = $2 AND currency = sqlc.arg(currency);

-- name: ListBelowThresholdMoneyAccounts :many
-- Money-in workers (#239/#240): accounts whose available balance
-- (balance - held) is under their configured low-balance threshold.
SELECT s.tenant_id, s.tenant_subject_id, s.currency,
       (COALESCE(b.balance, 0) - COALESCE(b.held_balance, 0))::bigint AS available,
       COALESCE(s.low_balance_threshold_micros, 0)::bigint AS threshold,
       s.auto_topup_enabled, s.auto_topup_amount_cents, s.auto_topup_payment_method_id,
       s.last_alert_at, s.last_topup_at
FROM openrails.money_accounts s
LEFT JOIN openrails.money_balances b
  ON b.tenant_id = s.tenant_id
 AND b.tenant_subject_id = s.tenant_subject_id
 AND b.currency = s.currency
WHERE s.tenant_id = $1
  AND s.low_balance_threshold_micros IS NOT NULL
  AND (COALESCE(b.balance, 0) - COALESCE(b.held_balance, 0)) < s.low_balance_threshold_micros;

-- name: ListChargeableArrearsMoneyAccounts :many
-- Arrears collection (#241): accounts owing with a card on file.
-- min_threshold <= 0 means "charge every account with owed > 0".
SELECT s.tenant_id, s.tenant_subject_id, s.currency,
       s.outstanding_owed_micros, s.auto_topup_payment_method_id
FROM openrails.money_accounts s
WHERE s.tenant_id = $1
  AND s.billing_mode = 'arrears'
  AND s.outstanding_owed_micros > 0
  AND s.auto_topup_payment_method_id IS NOT NULL
  AND (sqlc.arg(min_threshold)::bigint <= 0 OR s.outstanding_owed_micros >= sqlc.arg(min_threshold)::bigint);

-- name: GetMoneySpendLimit :one
SELECT * FROM openrails.money_spend_limits
WHERE tenant_id = $1 AND tenant_subject_id = $2 AND currency = sqlc.arg(currency) AND actor = $3
LIMIT 1;

-- name: UpsertMoneySpendLimit :exec
INSERT INTO openrails.money_spend_limits (
    id, tenant_id, tenant_subject_id, currency, actor,
    max_spend_per_day_micros, max_spend_per_month_micros, created_at, updated_at
) VALUES ($1, $2, $3, sqlc.arg(currency), $4, $5, $6, $7, $8)
ON CONFLICT (tenant_id, tenant_subject_id, currency, actor) DO UPDATE SET
    max_spend_per_day_micros = EXCLUDED.max_spend_per_day_micros,
    max_spend_per_month_micros = EXCLUDED.max_spend_per_month_micros,
    updated_at = EXCLUDED.updated_at;
