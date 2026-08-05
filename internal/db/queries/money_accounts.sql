-- openrails.money_settings: per-(tenant, payer, currency) spend policy + money-in
-- state (#237/#239/#240/#241/#298/#299/#302). amounts use the currency's internal
-- precision. currency is a system code; the Go registry is authority.

-- name: ListMoneyAccountPairs :many
-- Distinct (payer, currency) pairs to finalize invoices for (#472). The #512
-- ledger transfers are the durable source of every payer with money activity,
-- in every currency (the single-entry money_transactions table is gone).
SELECT customer_id::uuid AS customer_id, currency, MIN(created_at)::timestamptz AS period_anchor
FROM openrails.ledger_transfers
WHERE merchant_id = $1 AND customer_id IS NOT NULL
GROUP BY customer_id, currency
ORDER BY customer_id, currency;

-- name: GetMoneyAccountSettings :one
SELECT * FROM openrails.money_settings
WHERE merchant_id = $1 AND customer_id = $2 AND currency = sqlc.arg(currency)
LIMIT 1;

-- name: GetAdmissionCapacity :one
-- Hot-path affordability snapshot for service admit. The customer_balance account
-- carries Phase-H O(1) counters; money_settings is optional (missing = prepaid,
-- no credit line). Request in-flight holds live in Redis and are subtracted by
-- spendgate, not here.
SELECT
    (a.credits_posted - a.debits_posted)::bigint AS balance,
    -- Holds are Redis-only (#513); the durable ledger has no pending balance, so
    -- held is always 0 here (the two-phase pending columns were dropped, #512).
    0::bigint AS held,
    COALESCE(s.billing_mode, 'prepaid')::text AS billing_mode,
    COALESCE(s.credit_limit_amount, 0)::bigint AS credit_limit_amount,
    -- or#897: the payer's OWN arrears account, so outstanding owed stays part of
    -- the same O(1) point lookup. Debt is a negative arrears balance, so the
    -- exposure is (debits - credits), floored at 0.
    COALESCE(GREATEST(ar.debits_posted - ar.credits_posted, 0), 0)::bigint AS outstanding_owed
FROM openrails.ledger_accounts a
LEFT JOIN openrails.money_settings s
  ON s.merchant_id = a.merchant_id
 AND s.customer_id = a.customer_id
 AND s.currency = a.currency
LEFT JOIN openrails.ledger_accounts ar
  ON ar.merchant_id = a.merchant_id
 AND ar.customer_id = a.customer_id
 AND ar.currency = a.currency
 AND ar.account_type = 'arrears_liability'
WHERE a.merchant_id = sqlc.arg(merchant_id)::uuid
  AND a.customer_id = sqlc.arg(customer_id)::uuid
  AND a.currency = sqlc.arg(currency)::text
  AND a.account_type = 'customer_balance'
LIMIT 1;

-- name: LockMoneyAccountSettings :one
SELECT * FROM openrails.money_settings
WHERE merchant_id = $1 AND customer_id = $2 AND currency = sqlc.arg(currency)
FOR UPDATE;

-- name: InsertMoneyAccountSettingsIfAbsent :exec
-- Default settings row in the given billing mode; no-op when the row exists.
INSERT INTO openrails.money_settings (
    merchant_id, customer_id, currency, billing_mode, created_at, updated_at
) VALUES ($1, $2, sqlc.arg(currency), $3, sqlc.arg(now), sqlc.arg(now))
ON CONFLICT (merchant_id, customer_id, currency) DO NOTHING;

-- name: UpsertMoneyAccountSettings :exec
INSERT INTO openrails.money_settings (
    merchant_id, customer_id, currency, billing_mode,
    low_balance_threshold, auto_topup_enabled, auto_topup_amount,
    auto_topup_payment_method_id, default_credit_expiry_hours,
    created_at, updated_at
) VALUES ($1, $2, sqlc.arg(currency), $3, $4, $5, $6, $7, $8, $9, $10)
ON CONFLICT (merchant_id, customer_id, currency) DO UPDATE SET
    billing_mode = EXCLUDED.billing_mode,
    low_balance_threshold = EXCLUDED.low_balance_threshold,
    auto_topup_enabled = EXCLUDED.auto_topup_enabled,
    auto_topup_amount = EXCLUDED.auto_topup_amount,
    auto_topup_payment_method_id = EXCLUDED.auto_topup_payment_method_id,
    default_credit_expiry_hours = EXCLUDED.default_credit_expiry_hours,
    updated_at = EXCLUDED.updated_at;

-- name: SetMoneyAccountCreditLimit :exec
-- Admin-only arrears credit-line setter (#489). NOT part of the self-serve
-- UpsertMoneyAccountSettings — an operator path calls this. The settings row must
-- already exist (the caller ensures it).
UPDATE openrails.money_settings
SET credit_limit_amount = sqlc.arg(credit_limit)::bigint, updated_at = sqlc.arg(now)
WHERE merchant_id = $1 AND customer_id = $2 AND currency = sqlc.arg(currency);

-- name: SetMoneyAccountCollectionPaymentMethod :execrows
UPDATE openrails.money_settings
SET collection_payment_method_id = sqlc.arg(payment_method_id),
    updated_at = sqlc.arg(now)
WHERE merchant_id = $1
  AND customer_id = $2
  AND currency = sqlc.arg(currency);

-- name: StampMoneyAccountTopupAt :exec
UPDATE openrails.money_settings
SET last_topup_at = sqlc.arg(now), updated_at = sqlc.arg(now)
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
-- (#512 ledger customer_balance) is under their configured low-balance
-- threshold. Request holds live in Redis and are not part of this durable scan.
WITH avail AS (
    SELECT s.merchant_id, s.customer_id, s.currency,
           s.low_balance_threshold, s.auto_topup_enabled, s.auto_topup_amount,
           s.auto_topup_payment_method_id, s.last_topup_at,
           COALESCE((
               SELECT a.credits_posted - a.debits_posted
               FROM openrails.ledger_accounts a
               WHERE a.merchant_id = s.merchant_id AND a.customer_id = s.customer_id
                 AND a.currency = s.currency AND a.account_type = 'customer_balance'
           ), 0)::bigint AS available
    FROM openrails.money_settings s
    WHERE s.merchant_id = $1 AND s.low_balance_threshold IS NOT NULL
)
SELECT merchant_id, customer_id, currency, available,
       COALESCE(low_balance_threshold, 0)::bigint AS threshold,
       auto_topup_enabled, auto_topup_amount, auto_topup_payment_method_id,
       last_topup_at
FROM avail
WHERE available < low_balance_threshold;
