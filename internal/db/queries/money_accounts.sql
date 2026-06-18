-- openrails.money_settings: per-(tenant, payer, currency) spend policy + money-in
-- state (#237/#239/#240/#241/#298/#299/#302). amounts use the currency's internal
-- precision. currency is a system code; the Go registry is authority.

-- name: ListMoneyAccountPairs :many
-- Distinct (payer, currency) pairs to finalize invoices for (#472). The #512
-- ledger transfers are the durable source of every payer with money activity,
-- in every currency (the single-entry money_transactions table is gone).
SELECT DISTINCT customer_id::uuid AS customer_id, currency
FROM openrails.ledger_transfers
WHERE merchant_id = $1 AND customer_id IS NOT NULL
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
    max_spend_per_day, max_spend_per_month, max_outstanding_owed_amount,
    low_balance_threshold, auto_topup_enabled, auto_topup_amount_cents,
    auto_topup_payment_method_id, default_credit_expiry_days,
    hard_stop_on_breach, alert_threshold_pct, created_at, updated_at
) VALUES ($1, $2, $3, sqlc.arg(currency), $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)
ON CONFLICT (merchant_id, customer_id, currency) DO UPDATE SET
    billing_mode = EXCLUDED.billing_mode,
    max_spend_per_day = EXCLUDED.max_spend_per_day,
    max_spend_per_month = EXCLUDED.max_spend_per_month,
    max_outstanding_owed_amount = EXCLUDED.max_outstanding_owed_amount,
    low_balance_threshold = EXCLUDED.low_balance_threshold,
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
SET credit_limit_amount = sqlc.arg(credit_limit)::bigint, updated_at = sqlc.arg(now)
WHERE merchant_id = $1 AND customer_id = $2 AND currency = sqlc.arg(currency);

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
-- (#512 ledger customer_balance − durable open-window unsettled) is under their
-- configured low-balance threshold. Request holds live in Redis and are not part
-- of this durable account scan.
WITH avail AS (
    SELECT s.merchant_id, s.customer_id, s.currency,
           s.low_balance_threshold, s.auto_topup_enabled, s.auto_topup_amount_cents,
           s.auto_topup_payment_method_id, s.last_alert_at, s.last_topup_at,
           (
             COALESCE((
                 SELECT SUM(CASE WHEN t.credit_account_id = a.id THEN t.amount
                                 WHEN t.debit_account_id = a.id THEN -t.amount ELSE 0 END)
                 FROM openrails.ledger_accounts a
                 JOIN openrails.ledger_transfers t
                   ON t.merchant_id = a.merchant_id
                  AND (t.debit_account_id = a.id OR t.credit_account_id = a.id)
                  AND t.phase IN ('posted', 'post_pending')
                 WHERE a.merchant_id = s.merchant_id AND a.customer_id = s.customer_id
                   AND a.currency = s.currency AND a.account_type = 'customer_balance'
             ), 0)
           - COALESCE((SELECT SUM(w.held_amount - w.settled_amount) FROM openrails.money_windows w
                       WHERE w.merchant_id = s.merchant_id AND w.customer_id = s.customer_id
                         AND w.currency = s.currency AND w.status = 'open'), 0)
           )::bigint AS available
    FROM openrails.money_settings s
    WHERE s.merchant_id = $1 AND s.low_balance_threshold IS NOT NULL
)
SELECT merchant_id, customer_id, currency, available,
       COALESCE(low_balance_threshold, 0)::bigint AS threshold,
       auto_topup_enabled, auto_topup_amount_cents, auto_topup_payment_method_id,
       last_alert_at, last_topup_at
FROM avail
WHERE available < low_balance_threshold;
