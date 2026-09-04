-- name: ListMerchantInvoices :many
SELECT * FROM openrails.invoices
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND (sqlc.narg(customer_id)::uuid IS NULL OR customer_id = sqlc.narg(customer_id)::uuid)
  AND (sqlc.narg(currency)::text IS NULL OR currency = sqlc.narg(currency)::text)
  AND (sqlc.narg(status)::text IS NULL OR status = sqlc.narg(status)::text)
  AND (sqlc.narg(period_from)::timestamptz IS NULL OR period_from >= sqlc.narg(period_from)::timestamptz)
  AND (sqlc.narg(period_to)::timestamptz IS NULL OR period_from < sqlc.narg(period_to)::timestamptz)
ORDER BY period_from DESC, id DESC
LIMIT sqlc.arg(page_limit)::int OFFSET sqlc.arg(page_offset)::int;

-- name: CountMerchantInvoices :one
SELECT count(*) FROM openrails.invoices
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND (sqlc.narg(customer_id)::uuid IS NULL OR customer_id = sqlc.narg(customer_id)::uuid)
  AND (sqlc.narg(currency)::text IS NULL OR currency = sqlc.narg(currency)::text)
  AND (sqlc.narg(status)::text IS NULL OR status = sqlc.narg(status)::text)
  AND (sqlc.narg(period_from)::timestamptz IS NULL OR period_from >= sqlc.narg(period_from)::timestamptz)
  AND (sqlc.narg(period_to)::timestamptz IS NULL OR period_from < sqlc.narg(period_to)::timestamptz);

-- name: GetMerchantInvoice :one
SELECT * FROM openrails.invoices
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid;

-- name: InvoiceProfileCustomerExists :one
SELECT EXISTS (SELECT 1 FROM openrails.customers WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(customer_id)::uuid);
