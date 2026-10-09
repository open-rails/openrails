-- name: ListInvoicesPage :many
-- One page of invoices, newest period first, after a (period_starts_at, id)
-- cursor; every filter is optional.
SELECT * FROM billing.invoices
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND (sqlc.narg(customer_id)::uuid IS NULL OR customer_id = sqlc.narg(customer_id)::uuid)
  AND (sqlc.narg(currency)::text IS NULL OR currency = sqlc.narg(currency)::text)
  AND (sqlc.narg(status)::text IS NULL OR status = sqlc.narg(status)::text)
  AND (sqlc.narg(period_starts_after)::timestamptz IS NULL OR period_starts_at >= sqlc.narg(period_starts_after)::timestamptz)
  AND (sqlc.narg(period_starts_before)::timestamptz IS NULL OR period_starts_at < sqlc.narg(period_starts_before)::timestamptz)
  AND (sqlc.narg(after_at)::timestamptz IS NULL
       OR (period_starts_at, id) < (sqlc.narg(after_at)::timestamptz, sqlc.narg(after_id)::uuid))
ORDER BY period_starts_at DESC, id DESC
LIMIT sqlc.arg(row_limit)::int;

-- name: ListInvoicesByIDs :many
SELECT * FROM billing.invoices
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = ANY(sqlc.arg(ids)::uuid[])
ORDER BY period_starts_at DESC, id DESC;

-- name: GetMerchantInvoice :one
SELECT * FROM billing.invoices
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid;
