-- openrails.money_settings: per-(tenant, payer, currency) spend policy + money-in
-- state (#237/#239/#240/#241/#298/#299/#302). amounts use the currency's internal
-- precision. currency is a system code; the Go registry is authority.

-- name: GetMoneyAccountSettings :one
SELECT * FROM openrails.money_settings
WHERE merchant_id = $1 AND customer_id = $2 AND currency = sqlc.arg(currency)
LIMIT 1;

-- name: ListMoneyAccountSettingsByCustomer :many
SELECT * FROM openrails.money_settings
WHERE merchant_id = $1 AND customer_id = $2
ORDER BY currency;

-- name: GetAdmissionCapacity :one
-- Hot-path affordability snapshot for service admit. The customer_balance account
-- carries Phase-H O(1) counters; money_settings is optional (missing = prepaid,
-- no credit line). Redis request-admission holds are subtracted by spendgate;
-- durable operation authorizations are financial reservations and therefore
-- travel in this Postgres snapshot.
SELECT
    (a.credits_posted - a.debits_posted)::bigint AS balance,
    openrails.financial_held_amount(a.merchant_id, a.customer_id, a.currency, sqlc.arg(as_of)::timestamptz)::bigint AS held,
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
    created_at, updated_at
) VALUES ($1, $2, sqlc.arg(currency), $3, $4, $5)
ON CONFLICT (merchant_id, customer_id, currency) DO UPDATE SET
    billing_mode = EXCLUDED.billing_mode,
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

-- name: SetMoneyAccountTier :exec
-- Sets the host-assigned account tier.
UPDATE openrails.money_settings
SET tier = sqlc.arg(tier)::text, updated_at = sqlc.arg(now)
WHERE merchant_id = $1 AND customer_id = $2 AND currency = sqlc.arg(currency);

