-- name: SetOrderCheckout :execrows
-- A merchant order takes its one checkout while open.
UPDATE billing.orders
SET checkout_secret_hash = sqlc.arg(secret_hash)::bytea,
    checkout_expires_at = sqlc.arg(expires_at)::timestamptz,
    checkout_success_url = sqlc.arg(success_url)::text,
    checkout_cancel_url = sqlc.narg(cancel_url)::text,
    checkout_saved_payment_methods = sqlc.arg(saved_payment_methods)::boolean,
    updated_at = sqlc.arg(now)::timestamptz
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid
  AND origin = 'merchant' AND status = 'open' AND checkout_secret_hash IS NULL;

-- Credential directory: resolve only a complete secret's hash before any
-- merchant connection is pinned. Two rows reveal ambiguity, never a choice.
-- name: ResolveOrderCheckoutMerchant :many
SELECT o.merchant_id
FROM billing.orders o
JOIN billing.merchants m ON m.id = o.merchant_id
WHERE o.checkout_secret_hash = sqlc.arg(secret_hash)::bytea
  AND m.deleted_at IS NULL AND m.status = 'active'
LIMIT 2;

-- name: GetOrderCheckout :one
SELECT id, customer_id, checkout_expires_at, checkout_success_url, checkout_cancel_url, checkout_saved_payment_methods
FROM billing.orders
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND checkout_secret_hash = sqlc.arg(secret_hash)::bytea;
