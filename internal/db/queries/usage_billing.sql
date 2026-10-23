-- name: EnsureUsageBillingAccount :one
INSERT INTO billing.money_settings (merchant_id, customer_id, currency, billing_mode,
    usage_billing_anchor, usage_next_bill_at, created_at, updated_at)
VALUES (sqlc.arg(merchant_id), sqlc.arg(customer_id), sqlc.arg(currency), 'arrears',
    sqlc.arg(anchor)::timestamptz, sqlc.arg(next_bill_at)::timestamptz, sqlc.arg(now)::timestamptz, sqlc.arg(now)::timestamptz)
ON CONFLICT (merchant_id, customer_id, currency) DO UPDATE SET
    usage_billing_anchor = COALESCE(billing.money_settings.usage_billing_anchor, EXCLUDED.usage_billing_anchor),
    usage_next_bill_at = COALESCE(billing.money_settings.usage_next_bill_at, EXCLUDED.usage_next_bill_at)
RETURNING *;

-- name: UpdateUsageBillingAccount :exec
UPDATE billing.money_settings SET
    usage_billing_anchor = CASE WHEN sqlc.arg(set_anchor)::boolean THEN sqlc.narg(anchor)::timestamptz ELSE usage_billing_anchor END,
    usage_next_bill_at = CASE WHEN sqlc.arg(set_anchor)::boolean THEN sqlc.narg(next_bill_at)::timestamptz ELSE usage_next_bill_at END,
    usage_minimum_invoice_amount = CASE WHEN sqlc.arg(set_minimum)::boolean THEN sqlc.narg(minimum_invoice_amount)::bigint ELSE usage_minimum_invoice_amount END,
    updated_at = sqlc.arg(now)::timestamptz
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND customer_id = sqlc.arg(customer_id)::uuid AND currency = sqlc.arg(currency)::text;

-- name: InheritMoneyAccountCreditLimit :exec
UPDATE billing.money_settings SET credit_limit_override = false, updated_at = sqlc.arg(now)::timestamptz
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND customer_id = sqlc.arg(customer_id)::uuid AND currency = sqlc.arg(currency)::text;

-- name: FlagUsageCollectionDue :exec
UPDATE billing.money_settings SET usage_collection_due = true
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND customer_id = sqlc.arg(customer_id)::uuid AND currency = sqlc.arg(currency)::text;

-- name: ListUsageBillingDueAccounts :many
SELECT s.customer_id, s.currency
FROM billing.money_settings s
WHERE s.merchant_id = sqlc.arg(merchant_id)::uuid AND sqlc.arg(limits)::jsonb ? s.currency
    AND (s.customer_id,s.currency) > (sqlc.arg(after_customer)::uuid,sqlc.arg(after_currency)::text) AND (
    s.usage_collection_due OR s.usage_next_bill_at <= sqlc.arg(now)::timestamptz OR
    EXISTS (SELECT 1 FROM billing.ledger_accounts ar
        WHERE ar.merchant_id=s.merchant_id AND ar.customer_id=s.customer_id AND ar.currency=s.currency AND ar.account_type='arrears_liability'
        AND ar.debits_posted-ar.credits_posted >= CASE WHEN s.credit_limit_override THEN s.credit_limit_amount ELSE (sqlc.arg(limits)::jsonb->>s.currency)::bigint END
        AND ar.debits_posted-ar.credits_posted > 0
        AND ar.debits_posted-ar.credits_posted >= COALESCE(s.usage_minimum_invoice_amount,(sqlc.arg(minimums)::jsonb->>s.currency)::bigint,0)
        AND EXISTS (SELECT 1 FROM billing.owed_items oi WHERE oi.merchant_id=s.merchant_id AND oi.customer_id=s.customer_id AND oi.currency=s.currency AND oi.status='pending' AND oi.order_id IS NULL
            HAVING COALESCE(SUM(oi.amount),0) >= GREATEST(1,COALESCE(s.usage_minimum_invoice_amount,(sqlc.arg(minimums)::jsonb->>s.currency)::bigint,0)))) OR
    (s.usage_next_bill_at IS NULL AND EXISTS (SELECT 1 FROM billing.owed_items oi
        WHERE oi.merchant_id=s.merchant_id AND oi.customer_id=s.customer_id AND oi.currency=s.currency
        AND oi.status='pending' AND oi.order_id IS NULL)))
ORDER BY s.customer_id, s.currency
LIMIT sqlc.arg(row_limit)::int;

-- name: CompleteUsageBillingCheck :exec
UPDATE billing.money_settings SET usage_collection_due = false,
    usage_next_bill_at = sqlc.arg(next_bill_at)::timestamptz, updated_at = sqlc.arg(now)::timestamptz
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
SELECT customer_id FROM billing.money_settings
WHERE merchant_id=sqlc.arg(merchant_id)::uuid AND currency=sqlc.arg(currency)::text
    AND CASE WHEN credit_limit_override THEN credit_limit_amount ELSE sqlc.arg(default_credit)::bigint END > 0
    AND COALESCE(usage_minimum_invoice_amount,sqlc.arg(default_minimum)::bigint) >
        CASE WHEN credit_limit_override THEN credit_limit_amount ELSE sqlc.arg(default_credit)::bigint END
ORDER BY customer_id LIMIT 1;

-- name: LockUsagePolicy :exec
SELECT pg_advisory_lock(hashtext(current_database()),hashtext(sqlc.arg(policy_key)::text));
