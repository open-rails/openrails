-- openrails.invoices: monthly itemized statements (#303), immutable once
-- finalized; one per (tenant, payer, period).

-- name: GetInvoiceByPeriod :one
-- Idempotency key is per (payer, period, currency): one invoice per currency (#474).
SELECT * FROM openrails.invoices
WHERE merchant_id = $1 AND customer_id = $2
  AND period_from = $3 AND period_to = $4 AND currency = sqlc.arg(currency)
LIMIT 1;

-- name: InsertInvoice :exec
INSERT INTO openrails.invoices (
    id, merchant_id, customer_id, currency,
    period_from, period_to, usage_total, deposits_total, owed_accrued, owed_paid,
    closing_balance, line_items, money_movements, status, finalized_at, created_at, updated_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, COALESCE(sqlc.arg(line_items), '[]'::jsonb), COALESCE(sqlc.arg(money_movements), '{}'::jsonb), $14, $15, $16, $17);

-- name: ListInvoicesByPayer :many
SELECT * FROM openrails.invoices
WHERE merchant_id = $1 AND customer_id = $2
ORDER BY period_from DESC
LIMIT $3::int OFFSET $4::int;

-- name: CountInvoicesByPayer :one
SELECT count(*) FROM openrails.invoices
WHERE merchant_id = $1 AND customer_id = $2;

-- name: GetInvoiceForPayer :one
SELECT * FROM openrails.invoices
WHERE merchant_id = $1 AND customer_id = $2 AND id = $3
LIMIT 1;
