-- billing.invoices: monthly itemized statements (#303), immutable once
-- finalized; one per (tenant, payer, credit_type, period).

-- name: GetInvoiceByPeriod :one
SELECT * FROM billing.invoices
WHERE tenant_id = $1 AND tenant_subject_id = $2 AND credit_type_id = $3
  AND period_from = $4 AND period_to = $5
LIMIT 1;

-- name: InsertInvoice :exec
INSERT INTO billing.invoices (
    id, tenant_id, tenant_subject_id, credit_type_id, currency,
    period_from, period_to, usage_total, deposits_total, owed_accrued, owed_paid,
    closing_balance, line_items, money_movements, status, finalized_at, created_at, updated_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, COALESCE(sqlc.arg(line_items), '[]'::jsonb), COALESCE(sqlc.arg(money_movements), '{}'::jsonb), $15, $16, $17, $18);

-- name: ListInvoicesByPayer :many
SELECT * FROM billing.invoices
WHERE tenant_id = $1 AND tenant_subject_id = $2
ORDER BY period_from DESC
LIMIT $3::int OFFSET $4::int;

-- name: CountInvoicesByPayer :one
SELECT count(*) FROM billing.invoices
WHERE tenant_id = $1 AND tenant_subject_id = $2;

-- name: GetInvoiceForPayer :one
SELECT * FROM billing.invoices
WHERE tenant_id = $1 AND tenant_subject_id = $2 AND id = $3
LIMIT 1;

-- name: ListCreditAccountPairs :many
-- Every known (payer, credit_type) account in the tenant (= has a balance
-- row), for the due-invoice sweep.
SELECT b.tenant_subject_id, ct.name AS credit_type_name
FROM billing.credit_balances b
JOIN billing.credit_types ct ON ct.id = b.credit_type_id
WHERE b.tenant_id = $1
GROUP BY b.tenant_subject_id, ct.name;
