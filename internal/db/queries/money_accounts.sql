-- billing.money_settings: per-(merchant, customer, currency) spend policy and
-- money-in state. Amounts use the currency's registry scale; currency is a
-- system code.

-- name: GetMoneyAccountSettings :one
SELECT * FROM billing.money_settings
WHERE merchant_id = $1 AND customer_id = $2 AND currency = sqlc.arg(currency)
LIMIT 1;

-- name: ListMoneyAccountSettingsByCustomer :many
SELECT * FROM billing.money_settings
WHERE merchant_id = $1 AND customer_id = $2
ORDER BY currency;

-- name: GetAdmissionCapacity :one
-- Affordability snapshot: customer_units counters, pending usage and
-- running-charge accrual (as GetUnpostedAmounts reads them), money_settings
-- (missing = prepaid, no credit line) and outstanding owed.
SELECT
    (a.credits_posted - a.debits_posted)::bigint AS balance,
    COALESCE((SELECT SUM(p.amount) FROM billing.usage_pending_amounts p
              WHERE p.merchant_id = a.merchant_id AND p.customer_id = a.customer_id AND p.currency = a.currency), 0)::bigint AS pending,
    COALESCE((SELECT SUM(GREATEST(billing.running_charge_total(rc.rate_per_hour, rc.started_at, sqlc.arg(as_of)::timestamptz) - rc.posted_amount, 0))
              FROM billing.running_charges rc
             WHERE rc.merchant_id = a.merchant_id AND rc.customer_id = a.customer_id AND rc.currency = a.currency AND rc.stopped_at IS NULL), 0)::bigint AS accrued,
    -- A revoked collection agreement leaves only the balance to spend.
    CASE WHEN s.collection_revoked_at IS NULL THEN COALESCE(s.billing_mode, 'prepaid') ELSE 'prepaid' END::text AS billing_mode,
    COALESCE(s.credit_limit_amount, 0)::bigint AS credit_limit_amount,
    COALESCE(s.credit_limit_override, false)::boolean AS credit_limit_override,
    -- The customer's own arrears account, in the same point lookup. Debt is a
    -- negative arrears balance, so exposure is (debits - credits), floored at 0.
    COALESCE(GREATEST(ar.debits_posted - ar.credits_posted, 0), 0)::bigint AS outstanding_owed
FROM billing.ledger_accounts a
LEFT JOIN billing.money_settings s
  ON s.merchant_id = a.merchant_id
 AND s.customer_id = a.customer_id
 AND s.currency = a.currency
LEFT JOIN billing.ledger_accounts ar
  ON ar.merchant_id = a.merchant_id
 AND ar.customer_id = a.customer_id
 AND ar.currency = a.currency
 AND ar.account_type = 'customer_owed'
WHERE a.merchant_id = sqlc.arg(merchant_id)::uuid
  AND a.customer_id = sqlc.arg(customer_id)::uuid
  AND a.currency = sqlc.arg(currency)::text
  AND a.account_type = 'customer_units'
LIMIT 1;

-- name: ListViewCapacities :many
-- GetAdmissionCapacity for many customers in one currency, as gates' views
-- start from. A customer with no balance account has none.
SELECT
    c.id::uuid AS customer_id,
    COALESCE(a.credits_posted - a.debits_posted, 0)::bigint AS balance,
    COALESCE((SELECT SUM(p.amount) FROM billing.usage_pending_amounts p
              WHERE p.merchant_id = sqlc.arg(merchant_id)::uuid AND p.customer_id = c.id AND p.currency = sqlc.arg(currency)::text), 0)::bigint AS pending,
    COALESCE((SELECT SUM(GREATEST(billing.running_charge_total(rc.rate_per_hour, rc.started_at, sqlc.arg(as_of)::timestamptz) - rc.posted_amount, 0))
              FROM billing.running_charges rc
             WHERE rc.merchant_id = sqlc.arg(merchant_id)::uuid AND rc.customer_id = c.id AND rc.currency = sqlc.arg(currency)::text AND rc.stopped_at IS NULL), 0)::bigint AS accrued,
    CASE WHEN s.collection_revoked_at IS NULL THEN COALESCE(s.billing_mode, 'prepaid') ELSE 'prepaid' END::text AS billing_mode,
    COALESCE(s.credit_limit_amount, 0)::bigint AS credit_limit_amount,
    COALESCE(s.credit_limit_override, false)::boolean AS credit_limit_override,
    COALESCE(GREATEST(ar.debits_posted - ar.credits_posted, 0), 0)::bigint AS outstanding_owed
FROM unnest(sqlc.arg(customer_ids)::uuid[]) AS c(id)
LEFT JOIN billing.ledger_accounts a
  ON a.merchant_id = sqlc.arg(merchant_id)::uuid AND a.customer_id = c.id
 AND a.currency = sqlc.arg(currency)::text AND a.account_type = 'customer_units'
LEFT JOIN billing.money_settings s
  ON s.merchant_id = sqlc.arg(merchant_id)::uuid AND s.customer_id = c.id AND s.currency = sqlc.arg(currency)::text
LEFT JOIN billing.ledger_accounts ar
  ON ar.merchant_id = sqlc.arg(merchant_id)::uuid AND ar.customer_id = c.id
 AND ar.currency = sqlc.arg(currency)::text AND ar.account_type = 'customer_owed';

-- name: LockMoneyAccountSettings :one
SELECT * FROM billing.money_settings
WHERE merchant_id = $1 AND customer_id = $2 AND currency = sqlc.arg(currency)
FOR UPDATE;

-- name: InsertMoneyAccountSettingsIfAbsent :execrows
-- Default settings row in the given billing mode; no-op when the row exists.
INSERT INTO billing.money_settings (
    merchant_id, customer_id, currency, billing_mode, created_at, updated_at
) VALUES ($1, $2, sqlc.arg(currency), $3, sqlc.arg(now), sqlc.arg(now))
ON CONFLICT (merchant_id, customer_id, currency) DO NOTHING;

-- name: UpsertMoneyAccountSettings :exec
INSERT INTO billing.money_settings (
    merchant_id, customer_id, currency, billing_mode,
    created_at, updated_at
) VALUES ($1, $2, sqlc.arg(currency), $3, $4, $5)
ON CONFLICT (merchant_id, customer_id, currency) DO UPDATE SET
    billing_mode = EXCLUDED.billing_mode,
    updated_at = EXCLUDED.updated_at;

-- name: SetMoneyAccountCreditLimit :exec
-- Admin-only credit-line setter, not part of UpsertMoneyAccountSettings. The
-- settings row must already exist.
UPDATE billing.money_settings
SET credit_limit_amount = sqlc.arg(credit_limit)::bigint, credit_limit_override = true, updated_at = sqlc.arg(now)
WHERE merchant_id = $1 AND customer_id = $2 AND currency = sqlc.arg(currency);

-- name: SetMoneyAccountDefaultPaymentMethod :execrows
UPDATE billing.money_settings
SET default_payment_method_id = sqlc.arg(payment_method_id),
    updated_at = sqlc.arg(now)
WHERE merchant_id = $1
  AND customer_id = $2
  AND currency = sqlc.arg(currency)
  AND set_config('openrails.subscription_history', sqlc.arg(history_attribution)::text, true) IS NOT NULL;

-- name: RevokeMoneyCollection :exec
-- The customer revoked its collection agreement in the currency.
UPDATE billing.money_settings
SET collection_revoked_at = sqlc.arg(now)::timestamptz, updated_at = sqlc.arg(now)::timestamptz
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND customer_id = sqlc.arg(customer_id)::uuid AND currency = sqlc.arg(currency)::text;

-- name: RestoreMoneyCollection :execrows
-- A new collection agreement in the currency: the merchant's billing mode
-- applies again.
UPDATE billing.money_settings
SET collection_revoked_at = NULL, updated_at = sqlc.arg(now)::timestamptz
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND customer_id = sqlc.arg(customer_id)::uuid AND currency = sqlc.arg(currency)::text
  AND collection_revoked_at IS NOT NULL;

-- name: SetMoneyAccountTier :exec
-- Sets the host-assigned account tier.
UPDATE billing.money_settings
SET tier = NULLIF(sqlc.arg(tier)::text, ''), updated_at = sqlc.arg(now)
WHERE merchant_id = $1 AND customer_id = $2 AND currency = sqlc.arg(currency);


-- name: ListCustomersBalanceCurrencies :many
-- Every currency each customer holds a balance, settings, pending charges or
-- an unpaid usage invoice in: at most one row per customer and registered
-- currency, which bounds row_limit.
SELECT DISTINCT customer_id::uuid AS customer_id, currency::text AS currency
FROM (
    SELECT customer_id, currency FROM billing.ledger_accounts
    WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND customer_id = ANY(sqlc.arg(customer_ids)::uuid[])
      AND account_type = 'customer_units'
    UNION ALL
    SELECT customer_id, currency FROM billing.money_settings
    WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND customer_id = ANY(sqlc.arg(customer_ids)::uuid[])
    UNION ALL
    SELECT customer_id, currency FROM billing.owed_items
    WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND customer_id = ANY(sqlc.arg(customer_ids)::uuid[])
      AND invoice_id IS NULL AND status = 'pending'
    UNION ALL
    SELECT customer_id, currency FROM billing.invoices
    WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND customer_id = ANY(sqlc.arg(customer_ids)::uuid[])
      AND origin = 'usage' AND status IN ('open', 'processing') AND amount_paid < total
) currencies
ORDER BY customer_id, currency
LIMIT sqlc.arg(row_limit)::int;

-- name: ListCustomersMoneyAccounts :many
-- The named customers' balance and arrears accounts with their O(1)
-- balances: at most two per customer and registered currency.
SELECT a.customer_id::uuid AS customer_id, a.currency, a.account_type, (a.credits_posted - a.debits_posted)::bigint AS balance
FROM billing.ledger_accounts a
WHERE a.merchant_id = sqlc.arg(merchant_id)::uuid AND a.customer_id = ANY(sqlc.arg(customer_ids)::uuid[])
  AND a.account_type IN ('customer_units', 'customer_owed')
ORDER BY a.customer_id, a.currency, a.account_type
LIMIT sqlc.arg(row_limit)::int;

-- name: ListCustomersMoneySettings :many
-- At most one row per customer and registered currency.
SELECT * FROM billing.money_settings
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND customer_id = ANY(sqlc.arg(customer_ids)::uuid[])
ORDER BY customer_id, currency
LIMIT sqlc.arg(row_limit)::int;

-- name: ListCustomerSettingsAccounts :many
-- The customers' credit limits and trust levels, by currency. A customer has
-- at most one row per registered currency, which bounds row_limit.
SELECT customer_id, currency, credit_limit_amount, credit_limit_override, tier FROM billing.money_settings
WHERE merchant_id = sqlc.arg(merchant_id) AND customer_id = ANY(sqlc.arg(customer_ids)::uuid[])
  AND (credit_limit_override OR tier IS NOT NULL)
ORDER BY customer_id, currency
LIMIT sqlc.arg(row_limit)::int;

-- name: ListDefaultPaymentMethodsByCustomers :many
-- Each customer's default card per currency.
SELECT customer_id, currency, default_payment_method_id::uuid AS default_payment_method_id
FROM billing.money_settings
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND customer_id = ANY(sqlc.arg(customer_ids)::uuid[])
  AND default_payment_method_id IS NOT NULL;

-- name: GetUnpostedAmounts :one
-- What the ledger has not taken of a customer's balance: pending usage at its
-- provisional prices, and what running charges accrued since their last
-- posting. A posting through an instant reads the accrual as of it
-- (accrued_as_of); every other read, as of as_of.
SELECT
    COALESCE((SELECT SUM(p.amount) FROM billing.usage_pending_amounts p
              WHERE p.merchant_id = sqlc.arg(merchant_id)::uuid AND p.customer_id = sqlc.arg(customer_id)::uuid AND p.currency = sqlc.arg(currency)::text), 0)::bigint AS pending,
    COALESCE((SELECT SUM(GREATEST(billing.running_charge_total(rc.rate_per_hour, rc.started_at, COALESCE(sqlc.narg(accrued_as_of)::timestamptz, sqlc.arg(as_of)::timestamptz)) - rc.posted_amount, 0))
              FROM billing.running_charges rc
             WHERE rc.merchant_id = sqlc.arg(merchant_id)::uuid AND rc.customer_id = sqlc.arg(customer_id)::uuid AND rc.currency = sqlc.arg(currency)::text AND rc.stopped_at IS NULL), 0)::bigint AS accrued;
