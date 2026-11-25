-- name: SetInvoicePayURL :execrows
-- A merchant invoice takes its one pay URL while open.
UPDATE billing.invoices
SET pay_url_secret_hash = sqlc.arg(secret_hash)::bytea,
    pay_url_expires_at = sqlc.arg(expires_at)::timestamptz,
    success_url = sqlc.arg(success_url)::text,
    cancel_url = sqlc.narg(cancel_url)::text,
    saved_payment_methods = sqlc.arg(saved_payment_methods)::boolean,
    embedded = sqlc.arg(embedded)::boolean,
    updated_at = sqlc.arg(now)::timestamptz
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid
  AND origin = 'merchant' AND status = 'open' AND pay_url_secret_hash IS NULL;

-- Credential directory: resolve only a complete secret's hash before any
-- merchant connection is pinned. Two rows reveal ambiguity, never a choice.
-- name: ResolveInvoicePayURLMerchant :many
SELECT o.merchant_id
FROM billing.invoices o
JOIN billing.merchants m ON m.id = o.merchant_id
WHERE o.pay_url_secret_hash = sqlc.arg(secret_hash)::bytea
  AND m.deleted_at IS NULL AND m.status = 'active'
LIMIT 2;

-- name: GetInvoicePayURL :one
SELECT id, customer_id, pay_url_expires_at, success_url, cancel_url, saved_payment_methods, embedded
FROM billing.invoices
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND pay_url_secret_hash = sqlc.arg(secret_hash)::bytea;

-- name: CountInvoiceDeclines :one
-- Card attempts on an invoice the provider declined.
SELECT count(*) FROM billing.invoice_attempts
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND invoice_id = sqlc.arg(invoice_id)::uuid AND status = 'failed'
  AND deleted_at IS NULL;

-- Credential directory: the merchant whose embedded pay URL an invoice
-- id names, to frame it by that merchant's origins. The id is not a
-- credential; two rows reveal ambiguity, never a choice.
-- name: ResolveEmbeddedPayURLMerchant :many
SELECT o.merchant_id
FROM billing.invoices o
JOIN billing.merchants m ON m.id = o.merchant_id
WHERE o.id = sqlc.arg(id)::uuid AND o.embedded
  AND m.deleted_at IS NULL AND m.status = 'active'
LIMIT 2;

-- Merchant directory: the live merchant an invoice id names, read before any
-- merchant connection is pinned: a wallet's Solana Pay request carries
-- nothing else. The id is not a credential; two rows reveal ambiguity, never
-- a choice.
-- name: GetInvoiceMerchant :many
SELECT o.merchant_id
FROM billing.invoices o
JOIN billing.merchants m ON m.id = o.merchant_id
WHERE o.id = sqlc.arg(id)::uuid
  AND m.deleted_at IS NULL AND m.status = 'active'
LIMIT 2;

-- name: SaveInvoicePayURLReceipt :exec
-- The creation retry recovers only this capability, never a cached invoice.
-- It commits with the invoice and its hash, and lives no longer than the pay URL.
INSERT INTO billing.idempotency_keys
    (merchant_id, operation, idempotency_key, status, token, result, lease_expires_at, expires_at)
VALUES (sqlc.arg(merchant_id)::uuid, 'pay_url_creation', sqlc.arg(invoice_id)::uuid::text,
    'succeeded', sqlc.arg(invoice_id)::uuid, jsonb_build_object('secret', sqlc.arg(secret)::text),
    sqlc.arg(now)::timestamptz, sqlc.arg(expires_at)::timestamptz);

-- name: SetMyInvoicePayURL :execrows
-- A customer's own open invoice takes its embedded pay URL, in place of the
-- one it had: the last secret stops working. It lasts as long as the invoice
-- and returns nowhere.
UPDATE billing.invoices
SET pay_url_secret_hash = sqlc.arg(secret_hash)::bytea,
    pay_url_expires_at = expires_at,
    success_url = NULL,
    cancel_url = NULL,
    saved_payment_methods = true,
    embedded = true,
    updated_at = sqlc.arg(now)::timestamptz
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid AND customer_id = sqlc.arg(customer_id)::uuid
  AND origin = 'customer' AND status = 'open' AND collection_owner = 'engine'
  AND expires_at > sqlc.arg(now)::timestamptz;
