-- openrails.money_settings: per-(tenant, payer, currency) spend policy + money-in
-- state (#237/#239/#240/#241/#298/#299/#302). amounts are the currency's minor unit
-- (default µ$). currency is a system code; the Go registry is authority.

-- name: ListMoneyAccountPairs :many
-- Distinct (payer, currency) pairs to finalize invoices for (#472). money_balances
-- is gone (#491); the durable ledger (money_transactions) is now the source of
-- every payer with money activity, in every currency.
SELECT DISTINCT customer_id, currency
FROM openrails.money_transactions
WHERE merchant_id = $1
ORDER BY customer_id, currency;

-- name: GetMoneyAccountSettings :one
SELECT * FROM openrails.money_settings
WHERE merchant_id = $1 AND customer_id = $2 AND currency = sqlc.arg(currency)
LIMIT 1;

-- name: LockMoneyAccountSettings :one
SELECT * FROM openrails.money_settings
WHERE merchant_id = $1 AND customer_id = $2 AND currency = sqlc.arg(currency)
FOR UPDATE;

-- name: InsertMoneyAccountSettingsIfAbsent :exec
-- Default settings row (hard-stop on, 80% alert threshold) in the given
-- billing mode; no-op when the row exists.
INSERT INTO openrails.money_settings (
    id, merchant_id, customer_id, currency, billing_mode,
    hard_stop_on_breach, alert_threshold_pct, created_at, updated_at
) VALUES ($1, $2, $3, sqlc.arg(currency), $4, true, 80, sqlc.arg(now), sqlc.arg(now))
ON CONFLICT (merchant_id, customer_id, currency) DO NOTHING;

-- name: UpsertMoneyAccountSettings :exec
INSERT INTO openrails.money_settings (
    id, merchant_id, customer_id, currency, billing_mode,
    max_spend_per_day_micros, max_spend_per_month_micros, max_outstanding_owed_micros,
    low_balance_threshold_micros, auto_topup_enabled, auto_topup_amount_cents,
    auto_topup_payment_method_id, default_credit_expiry_days,
    hard_stop_on_breach, alert_threshold_pct, created_at, updated_at
) VALUES ($1, $2, $3, sqlc.arg(currency), $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)
ON CONFLICT (merchant_id, customer_id, currency) DO UPDATE SET
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

-- name: SetMoneyAccountCreditLimit :exec
-- Admin-only arrears credit-line setter (#489). NOT part of the self-serve
-- UpsertMoneyAccountSettings — an operator path calls this. The settings row must
-- already exist (the caller ensures it).
UPDATE openrails.money_settings
SET credit_limit_micros = sqlc.arg(credit_limit)::bigint, updated_at = sqlc.arg(now)
WHERE merchant_id = $1 AND customer_id = $2 AND currency = sqlc.arg(currency);

-- name: AddMoneyOutstandingOwed :exec
UPDATE openrails.money_settings
SET outstanding_owed_micros = outstanding_owed_micros + sqlc.arg(amount)::bigint,
    updated_at = sqlc.arg(now)
WHERE merchant_id = $1 AND customer_id = $2 AND currency = sqlc.arg(currency);

-- name: ReduceMoneyOutstandingOwedSnapshot :execrows
-- CAS-style decrement: only applies when owed still covers the snapshot, so
-- two collection runs racing on the same snapshot can't double-decrement.
UPDATE openrails.money_settings
SET outstanding_owed_micros = GREATEST(0, outstanding_owed_micros - sqlc.arg(snapshot)::bigint),
    updated_at = sqlc.arg(now)
WHERE merchant_id = $1 AND customer_id = $2 AND currency = sqlc.arg(currency)
  AND outstanding_owed_micros >= sqlc.arg(snapshot)::bigint;

-- name: StampMoneyAccountAlertAt :exec
UPDATE openrails.money_settings
SET last_alert_at = sqlc.arg(now), updated_at = sqlc.arg(now)
WHERE merchant_id = $1 AND customer_id = $2 AND currency = sqlc.arg(currency);

-- name: StampMoneyAccountTopupAt :exec
UPDATE openrails.money_settings
SET last_topup_at = sqlc.arg(now), updated_at = sqlc.arg(now)
WHERE merchant_id = $1 AND customer_id = $2 AND currency = sqlc.arg(currency);

-- name: SetMoneyAccountPaymentVerified :exec
UPDATE openrails.money_settings
SET verified_payment_method = sqlc.arg(verified)::boolean,
    verified_at = CASE WHEN sqlc.arg(verified)::boolean THEN sqlc.arg(now)::timestamptz ELSE NULL END,
    updated_at = sqlc.arg(now)::timestamptz
WHERE merchant_id = $1 AND customer_id = $2 AND currency = sqlc.arg(currency);

-- name: SuspendMoneyAccount :exec
UPDATE openrails.money_settings
SET suspended_at = sqlc.arg(now)::timestamptz,
    suspend_reason = sqlc.narg(reason),
    updated_at = sqlc.arg(now)::timestamptz
WHERE merchant_id = $1 AND customer_id = $2 AND currency = sqlc.arg(currency);

-- name: ResumeMoneyAccount :exec
UPDATE openrails.money_settings
SET suspended_at = NULL, suspend_reason = NULL, updated_at = $3
WHERE merchant_id = $1 AND customer_id = $2 AND currency = sqlc.arg(currency);

-- name: SetMoneyAccountTier :exec
-- Sets the tier AND its source (#476). 'admin' = an explicit override that
-- auto-graduation must not overwrite; 'auto' = schedule-driven.
UPDATE openrails.money_settings
SET tier = sqlc.arg(tier)::text, tier_source = sqlc.arg(tier_source)::text, updated_at = sqlc.arg(now)
WHERE merchant_id = $1 AND customer_id = $2 AND currency = sqlc.arg(currency);

-- name: AutoGraduateMoneyAccountTier :exec
-- Auto-graduation write (#476): set tier_source='auto' and the new tier ONLY
-- when the current source is not 'admin' (an admin override always wins). The
-- caller computes the monotonic target tier; this guards against clobbering an
-- admin override at the DB level so a race cannot.
UPDATE openrails.money_settings
SET tier = sqlc.arg(tier)::text, tier_source = 'auto', updated_at = sqlc.arg(now)
WHERE merchant_id = $1 AND customer_id = $2 AND currency = sqlc.arg(currency)
  AND tier_source <> 'admin';

-- name: ListBelowThresholdMoneyAccounts :many
-- Money-in workers (#239/#240): accounts whose DERIVED available balance
-- (#491: SUM spendable blocks - active holds - open-window unsettled) is under
-- their configured low-balance threshold.
SELECT s.merchant_id, s.customer_id, s.currency,
       (
         COALESCE((SELECT SUM(mb.remaining_amount) FROM openrails.money_blocks mb
                   WHERE mb.merchant_id = s.merchant_id AND mb.customer_id = s.customer_id
                     AND mb.currency = s.currency AND mb.remaining_amount > 0
                     AND (mb.expires_at IS NULL OR mb.expires_at > sqlc.arg(now)::timestamptz)), 0)
       - COALESCE((SELECT SUM(COALESCE(t.authorized_amount, 0)) FROM openrails.money_transactions t
                   WHERE t.merchant_id = s.merchant_id AND t.customer_id = s.customer_id
                     AND t.currency = s.currency AND t.transaction_type = 'hold' AND t.status = 'active'), 0)
       - COALESCE((SELECT SUM(w.held_amount - w.settled_amount) FROM openrails.money_windows w
                   WHERE w.merchant_id = s.merchant_id AND w.customer_id = s.customer_id
                     AND w.currency = s.currency AND w.status = 'open'), 0)
       )::bigint AS available,
       COALESCE(s.low_balance_threshold_micros, 0)::bigint AS threshold,
       s.auto_topup_enabled, s.auto_topup_amount_cents, s.auto_topup_payment_method_id,
       s.last_alert_at, s.last_topup_at
FROM openrails.money_settings s
WHERE s.merchant_id = $1
  AND s.low_balance_threshold_micros IS NOT NULL
  AND (
         COALESCE((SELECT SUM(mb.remaining_amount) FROM openrails.money_blocks mb
                   WHERE mb.merchant_id = s.merchant_id AND mb.customer_id = s.customer_id
                     AND mb.currency = s.currency AND mb.remaining_amount > 0
                     AND (mb.expires_at IS NULL OR mb.expires_at > sqlc.arg(now)::timestamptz)), 0)
       - COALESCE((SELECT SUM(COALESCE(t.authorized_amount, 0)) FROM openrails.money_transactions t
                   WHERE t.merchant_id = s.merchant_id AND t.customer_id = s.customer_id
                     AND t.currency = s.currency AND t.transaction_type = 'hold' AND t.status = 'active'), 0)
       - COALESCE((SELECT SUM(w.held_amount - w.settled_amount) FROM openrails.money_windows w
                   WHERE w.merchant_id = s.merchant_id AND w.customer_id = s.customer_id
                     AND w.currency = s.currency AND w.status = 'open'), 0)
       ) < s.low_balance_threshold_micros;

-- name: ListChargeableArrearsMoneyAccounts :many
-- Arrears collection (#241): accounts owing with a card on file.
-- min_threshold <= 0 means "charge every account with owed > 0".
SELECT s.merchant_id, s.customer_id, s.currency,
       s.outstanding_owed_micros, s.auto_topup_payment_method_id
FROM openrails.money_settings s
WHERE s.merchant_id = $1
  AND s.billing_mode = 'arrears'
  AND s.outstanding_owed_micros > 0
  AND s.auto_topup_payment_method_id IS NOT NULL
  AND (sqlc.arg(min_threshold)::bigint <= 0 OR s.outstanding_owed_micros >= sqlc.arg(min_threshold)::bigint);

-- name: GetMoneySpendLimit :one
SELECT * FROM openrails.money_spend_limits
WHERE merchant_id = $1 AND customer_id = $2 AND currency = sqlc.arg(currency) AND actor = $3
LIMIT 1;

-- name: UpsertMoneySpendLimit :exec
INSERT INTO openrails.money_spend_limits (
    id, merchant_id, customer_id, currency, actor,
    max_spend_per_day_micros, max_spend_per_month_micros, created_at, updated_at
) VALUES ($1, $2, $3, sqlc.arg(currency), $4, $5, $6, $7, $8)
ON CONFLICT (merchant_id, customer_id, currency, actor) DO UPDATE SET
    max_spend_per_day_micros = EXCLUDED.max_spend_per_day_micros,
    max_spend_per_month_micros = EXCLUDED.max_spend_per_month_micros,
    updated_at = EXCLUDED.updated_at;
