-- billing.money_settings: per-(tenant, payer, currency) spend policy + money-in
-- state (#237/#239/#240/#241/#298/#299/#302). amounts use the currency's internal
-- precision. currency is a system code; the Go registry is authority.

-- name: GetMoneyAccountSettings :one
SELECT * FROM billing.money_settings
WHERE merchant_id = $1 AND customer_id = $2 AND currency = sqlc.arg(currency)
LIMIT 1;

-- name: ListMoneyAccountSettingsByCustomer :many
SELECT * FROM billing.money_settings
WHERE merchant_id = $1 AND customer_id = $2
ORDER BY currency;

-- name: GetAdmissionCapacity :one
-- Hot-path affordability snapshot for service admit. The customer_balance account
-- carries O(1) counters; money_settings is optional (missing = prepaid,
-- no credit line). Redis request-admission holds are subtracted by spendgate;
-- durable operation authorizations are financial reservations and therefore
-- travel in this Postgres snapshot.
SELECT
    (a.credits_posted - a.debits_posted)::bigint AS balance,
    -- Same hold total as GetFinancialHeldAmount.
    (COALESCE((SELECT SUM(oa.authorized_amount)
              FROM billing.operation_authorizations oa
             WHERE oa.merchant_id = a.merchant_id AND oa.customer_id = a.customer_id AND oa.currency = a.currency AND oa.state = 'open'), 0)
     + COALESCE((SELECT SUM(ao.estimated_amount)
              FROM billing.admission_operations ao
             WHERE ao.merchant_id = a.merchant_id AND ao.customer_id = a.customer_id AND ao.currency = a.currency AND ao.state = 'open'
               AND ao.admitted_at >= sqlc.arg(held_since)::timestamptz
               AND ao.expires_at > sqlc.arg(as_of)::timestamptz), 0)
     -- Retired credit funds its refund from the retired account. Only value
     -- still in customer_balance remains reserved here; an unswept expired
     -- source is entirely unavailable, including its unrefunded remainder.
     + COALESCE((SELECT sum(CASE WHEN g.ends_at <= sqlc.arg(as_of)::timestamptz
         THEN lot.remaining ELSE LEAST(ceil(g.amount::numeric * r.pending / p.amount),lot.remaining) END)
       FROM (SELECT refunded_payment_id, sum(-refund.amount)::numeric AS pending
         FROM billing.payments refund WHERE refund.merchant_id=a.merchant_id
           AND refund.customer_id=a.customer_id AND refund.currency=a.currency
           AND refund.status='pending' AND refund.amount<0 AND refund.deleted_at IS NULL
         GROUP BY refunded_payment_id) r
       JOIN billing.payments p ON p.merchant_id=a.merchant_id AND p.id=r.refunded_payment_id AND p.amount>0 AND p.deleted_at IS NULL
       JOIN billing.grants g ON g.merchant_id=a.merchant_id AND g.payment_id=p.id
         AND g.kind='credit' AND g.event='grant'
         AND g.spec_snapshot->'deposit'->'paid_amount' IS NOT NULL
       JOIN LATERAL (SELECT GREATEST(g.amount-COALESCE(sum(CASE WHEN lt.transfer_type='credit_refund_restore' THEN -lt.amount ELSE lt.amount END),0),0)::numeric AS remaining
         FROM billing.ledger_transfers lt WHERE lt.merchant_id=g.merchant_id AND lt.grant_id=g.id
           AND lt.transfer_type IN ('credit_spend','owed_repayment','credit_expire','credit_revoke','credit_refund','credit_refund_restore')) lot ON true),0))::bigint AS held,
    COALESCE(s.billing_mode, 'prepaid')::text AS billing_mode,
    COALESCE(s.credit_limit_amount, 0)::bigint AS credit_limit_amount,
    -- or#897: the payer's OWN arrears account, so outstanding owed stays part of
    -- the same O(1) point lookup. Debt is a negative arrears balance, so the
    -- exposure is (debits - credits), floored at 0.
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
 AND ar.account_type = 'arrears_liability'
WHERE a.merchant_id = sqlc.arg(merchant_id)::uuid
  AND a.customer_id = sqlc.arg(customer_id)::uuid
  AND a.currency = sqlc.arg(currency)::text
  AND a.account_type = 'customer_balance'
LIMIT 1;

-- name: LockMoneyAccountSettings :one
SELECT * FROM billing.money_settings
WHERE merchant_id = $1 AND customer_id = $2 AND currency = sqlc.arg(currency)
FOR UPDATE;

-- name: InsertMoneyAccountSettingsIfAbsent :exec
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
-- Admin-only arrears credit-line setter (#489). NOT part of the self-serve
-- UpsertMoneyAccountSettings — an operator path calls this. The settings row must
-- already exist (the caller ensures it).
UPDATE billing.money_settings
SET credit_limit_amount = sqlc.arg(credit_limit)::bigint, updated_at = sqlc.arg(now)
WHERE merchant_id = $1 AND customer_id = $2 AND currency = sqlc.arg(currency);

-- name: SetMoneyAccountCollectionPaymentMethod :execrows
UPDATE billing.money_settings
SET collection_payment_method_id = sqlc.arg(payment_method_id),
    updated_at = sqlc.arg(now)
WHERE merchant_id = $1
  AND customer_id = $2
  AND currency = sqlc.arg(currency);

-- name: SetMoneyAccountTier :exec
-- Sets the host-assigned account tier.
UPDATE billing.money_settings
SET tier = NULLIF(sqlc.arg(tier)::text, ''), updated_at = sqlc.arg(now)
WHERE merchant_id = $1 AND customer_id = $2 AND currency = sqlc.arg(currency);


-- Every currency a payer holds a balance, settings, pending items or an open invoice in.
-- name: ListCustomerBalanceCurrencies :many
SELECT currency::text AS currency
FROM (
    SELECT currency FROM billing.ledger_accounts
    WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND customer_id = sqlc.arg(customer_id)::uuid
      AND account_type = 'customer_balance'
    UNION
    SELECT currency FROM billing.money_settings
    WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND customer_id = sqlc.arg(customer_id)::uuid
    UNION
    SELECT currency FROM billing.invoice_items
    WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND customer_id = sqlc.arg(customer_id)::uuid
      AND invoice_id IS NULL AND status = 'pending'
    UNION
    SELECT currency FROM billing.invoices
    WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND customer_id = sqlc.arg(customer_id)::uuid
      AND status IN ('open', 'past_due') AND amount_due > 0
) currencies
ORDER BY currency;
