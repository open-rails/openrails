-- name: EnsureUsageBillingAccount :one
-- The account bills at its next bill date and is due no later.
INSERT INTO billing.money_settings (merchant_id, customer_id, currency, billing_mode,
    usage_billing_anchor, usage_next_bill_at, usage_due_at, created_at, updated_at)
VALUES (sqlc.arg(merchant_id), sqlc.arg(customer_id), sqlc.arg(currency), 'arrears',
    sqlc.arg(anchor)::timestamptz, sqlc.arg(next_bill_at)::timestamptz, sqlc.arg(next_bill_at)::timestamptz,
    sqlc.arg(now)::timestamptz, sqlc.arg(now)::timestamptz)
ON CONFLICT (merchant_id, customer_id, currency) DO UPDATE SET
    usage_billing_anchor = COALESCE(billing.money_settings.usage_billing_anchor, EXCLUDED.usage_billing_anchor),
    usage_next_bill_at = COALESCE(billing.money_settings.usage_next_bill_at, EXCLUDED.usage_next_bill_at),
    usage_due_at = LEAST(billing.money_settings.usage_due_at, COALESCE(billing.money_settings.usage_next_bill_at, EXCLUDED.usage_next_bill_at))
RETURNING *;

-- name: UpdateUsageBillingAccount :exec
UPDATE billing.money_settings SET
    usage_billing_anchor = CASE WHEN sqlc.arg(set_anchor)::boolean THEN sqlc.narg(anchor)::timestamptz ELSE usage_billing_anchor END,
    usage_next_bill_at = CASE WHEN sqlc.arg(set_anchor)::boolean THEN sqlc.narg(next_bill_at)::timestamptz ELSE usage_next_bill_at END,
    usage_due_at = CASE WHEN sqlc.arg(set_anchor)::boolean THEN LEAST(usage_due_at, sqlc.narg(next_bill_at)::timestamptz) ELSE usage_due_at END,
    usage_minimum_invoice_amount = CASE WHEN sqlc.arg(set_minimum)::boolean THEN sqlc.narg(minimum_invoice_amount)::bigint ELSE usage_minimum_invoice_amount END,
    updated_at = sqlc.arg(now)::timestamptz
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND customer_id = sqlc.arg(customer_id)::uuid AND currency = sqlc.arg(currency)::text;

-- name: InheritMoneyAccountCreditLimit :exec
UPDATE billing.money_settings SET credit_limit_override = false, updated_at = sqlc.arg(now)::timestamptz
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND customer_id = sqlc.arg(customer_id)::uuid AND currency = sqlc.arg(currency)::text;

-- name: FlagUsageCollectionDue :exec
-- Bill the period so far at once, whatever the threshold.
UPDATE billing.money_settings SET usage_collection_due = true, usage_due_at = LEAST(usage_due_at, sqlc.arg(now)::timestamptz)
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND customer_id = sqlc.arg(customer_id)::uuid AND currency = sqlc.arg(currency)::text;

-- name: ListDueUsageAccounts :many
-- CROSS-MERCHANT: the usage accounts due now, in due order. An idle pass reads
-- no row.
SELECT merchant_id, customer_id, currency, usage_due_at::timestamptz AS usage_due_at
FROM billing.money_settings
WHERE usage_due_at <= sqlc.arg(now)::timestamptz
  AND (sqlc.narg(after_at)::timestamptz IS NULL
       OR (usage_due_at, merchant_id, customer_id, currency)
          > (sqlc.narg(after_at)::timestamptz, sqlc.narg(after_merchant_id)::uuid, sqlc.narg(after_customer_id)::uuid, sqlc.narg(after_currency)::text))
ORDER BY usage_due_at, merchant_id, customer_id, currency
LIMIT sqlc.arg(row_limit)::int;

-- name: MarkUsageAccountsDue :exec
-- The merchant's usage terms changed: each of its accounts in a configured
-- currency whose arrears reach the resulting credit limit with enough
-- unbilled to bill, or with unbilled charges and no bill date, is checked on
-- the next pass.
WITH scoped AS (
    SELECT s.merchant_id, s.customer_id, s.currency, s.usage_next_bill_at, s.usage_minimum_invoice_amount,
        LEAST(CASE WHEN s.credit_limit_override THEN s.credit_limit_amount ELSE (sqlc.arg(limits)::jsonb->>s.currency)::bigint END,NULLIF((sqlc.arg(policies)::jsonb -> 'policies' -> COALESCE(
        CASE WHEN sqlc.arg(policies)::jsonb -> 'policies' -> c.billing_policy IS NOT NULL THEN c.billing_policy END,
        sqlc.arg(policies)::jsonb -> 'tiers' ->> s.tier,
        sqlc.arg(policies)::jsonb ->> 'default') ->> 'outstanding_cap_amount')::bigint,0)) AS effective_credit_limit
    FROM billing.money_settings s JOIN billing.customers c ON c.merchant_id=s.merchant_id AND c.id=s.customer_id
    WHERE s.merchant_id=sqlc.arg(merchant_id)::uuid AND sqlc.arg(limits)::jsonb ? s.currency
)
UPDATE billing.money_settings m SET usage_due_at = LEAST(m.usage_due_at, sqlc.arg(now)::timestamptz)
FROM scoped s
WHERE m.merchant_id = s.merchant_id AND m.customer_id = s.customer_id AND m.currency = s.currency AND (
    EXISTS (SELECT 1 FROM billing.ledger_accounts ar
        WHERE ar.merchant_id=s.merchant_id AND ar.customer_id=s.customer_id AND ar.currency=s.currency AND ar.account_type='arrears_liability'
        AND ar.debits_posted-ar.credits_posted >= s.effective_credit_limit
        AND ar.debits_posted-ar.credits_posted > 0
        AND ar.debits_posted-ar.credits_posted >= COALESCE(s.usage_minimum_invoice_amount,(sqlc.arg(minimums)::jsonb->>s.currency)::bigint,0)
        AND EXISTS (SELECT 1 FROM billing.owed_items oi WHERE oi.merchant_id=s.merchant_id AND oi.customer_id=s.customer_id AND oi.currency=s.currency AND oi.status='pending' AND oi.order_id IS NULL
            HAVING COALESCE(SUM(oi.amount),0) >= GREATEST(1,COALESCE(s.usage_minimum_invoice_amount,(sqlc.arg(minimums)::jsonb->>s.currency)::bigint,0)))) OR
    (s.usage_next_bill_at IS NULL AND EXISTS (SELECT 1 FROM billing.owed_items oi
        WHERE oi.merchant_id=s.merchant_id AND oi.customer_id=s.customer_id AND oi.currency=s.currency
        AND oi.status='pending' AND oi.order_id IS NULL)));

-- name: RebuildUsageDue :exec
-- A restored merchant's usage accounts are due again: at their bill date, or
-- at once when they were due to bill or have unbilled charges and no date.
UPDATE billing.money_settings s SET usage_due_at = CASE
    WHEN s.usage_collection_due THEN sqlc.arg(now)::timestamptz
    WHEN s.usage_next_bill_at IS NOT NULL THEN s.usage_next_bill_at
    ELSE sqlc.arg(now)::timestamptz END
WHERE s.merchant_id = sqlc.arg(merchant_id)::uuid
  AND (s.usage_collection_due OR s.usage_next_bill_at IS NOT NULL OR EXISTS (SELECT 1 FROM billing.owed_items oi
      WHERE oi.merchant_id = s.merchant_id AND oi.customer_id = s.customer_id AND oi.currency = s.currency
        AND oi.status = 'pending' AND oi.order_id IS NULL));

-- name: CompleteUsageBillingCheck :exec
-- A check done: the account is next due at its bill date, null with nothing
-- pending.
UPDATE billing.money_settings SET usage_collection_due = false,
    usage_next_bill_at = sqlc.narg(next_bill_at)::timestamptz, usage_due_at = sqlc.narg(next_bill_at)::timestamptz,
    updated_at = sqlc.arg(now)::timestamptz
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND customer_id = sqlc.arg(customer_id)::uuid AND currency = sqlc.arg(currency)::text;

-- name: ListCustomerUsageBilling :many
SELECT customer_id, currency, usage_billing_anchor, usage_next_bill_at, usage_minimum_invoice_amount
FROM billing.money_settings
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND customer_id = ANY(sqlc.arg(customer_ids)::uuid[])
    AND (usage_billing_anchor IS NOT NULL OR usage_minimum_invoice_amount IS NOT NULL)
ORDER BY customer_id, currency;

-- name: LatestUsageOrderEnd :one
SELECT COALESCE(MAX(period_ends_at),'0001-01-01 00:00:00+00'::timestamptz)::timestamptz AS period_end FROM billing.orders
WHERE merchant_id=sqlc.arg(merchant_id)::uuid AND customer_id=sqlc.arg(customer_id)::uuid
    AND currency=sqlc.arg(currency)::text AND origin='usage'
    AND period_starts_at=sqlc.arg(period_start)::timestamptz;

-- name: FindInvalidUsageMinimum :one
WITH scoped AS (
    SELECT s.customer_id, COALESCE(s.usage_minimum_invoice_amount,sqlc.arg(default_minimum)::bigint) AS minimum,
        LEAST(CASE WHEN s.credit_limit_override THEN s.credit_limit_amount ELSE sqlc.arg(default_credit)::bigint END,NULLIF((sqlc.arg(policies)::jsonb -> 'policies' -> COALESCE(
        CASE WHEN sqlc.arg(policies)::jsonb -> 'policies' -> c.billing_policy IS NOT NULL THEN c.billing_policy END,
        sqlc.arg(policies)::jsonb -> 'tiers' ->> s.tier,
        sqlc.arg(policies)::jsonb ->> 'default') ->> 'outstanding_cap_amount')::bigint,0)) AS credit_limit
    FROM billing.money_settings s JOIN billing.customers c ON c.merchant_id=s.merchant_id AND c.id=s.customer_id
    WHERE s.merchant_id=sqlc.arg(merchant_id)::uuid AND s.currency=sqlc.arg(currency)::text
)
SELECT customer_id FROM scoped WHERE credit_limit>0 AND minimum>credit_limit
ORDER BY customer_id LIMIT 1;

-- name: LockUsagePolicy :exec
SELECT pg_advisory_lock(hashtext(current_database()),hashtext(sqlc.arg(policy_key)::text));
