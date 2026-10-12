-- name: SetOrderCheckout :execrows
-- A merchant order takes its one checkout while open.
UPDATE billing.orders
SET checkout_secret_hash = sqlc.arg(secret_hash)::bytea,
    checkout_expires_at = sqlc.arg(expires_at)::timestamptz,
    checkout_success_url = sqlc.arg(success_url)::text,
    checkout_cancel_url = sqlc.narg(cancel_url)::text,
    checkout_saved_payment_methods = sqlc.arg(saved_payment_methods)::boolean,
    checkout_ui_mode = sqlc.arg(ui_mode)::text,
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
SELECT id, customer_id, checkout_expires_at, checkout_success_url, checkout_cancel_url, checkout_saved_payment_methods, checkout_ui_mode
FROM billing.orders
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND checkout_secret_hash = sqlc.arg(secret_hash)::bytea;

-- name: CountOrderDeclines :one
-- Card attempts on an order the provider declined.
SELECT count(*) FROM billing.checkout_attempts
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND order_id = sqlc.arg(order_id)::uuid AND status = 'failed'
  AND deleted_at IS NULL;

-- Credential directory: the merchant whose embedded checkout page an order
-- id names, to frame it by that merchant's origins. The id is not a
-- credential; two rows reveal ambiguity, never a choice.
-- name: ResolveEmbeddedCheckoutMerchant :many
SELECT o.merchant_id
FROM billing.orders o
JOIN billing.merchants m ON m.id = o.merchant_id
WHERE o.id = sqlc.arg(id)::uuid AND o.checkout_ui_mode = 'embedded'
  AND m.deleted_at IS NULL AND m.status = 'active'
LIMIT 2;

-- Merchant directory: the live merchant an order id names, read before any
-- merchant connection is pinned: a wallet's Solana Pay request carries
-- nothing else. The id is not a credential; two rows reveal ambiguity, never
-- a choice.
-- name: GetOrderMerchant :many
SELECT o.merchant_id
FROM billing.orders o
JOIN billing.merchants m ON m.id = o.merchant_id
WHERE o.id = sqlc.arg(id)::uuid
  AND m.deleted_at IS NULL AND m.status = 'active'
LIMIT 2;

-- name: SaveOrderCheckoutReceipt :exec
-- The creation retry recovers only this capability, never a cached order.
-- It commits with the order and its hash, and lives no longer than the checkout.
INSERT INTO billing.idempotency_keys
    (merchant_id, operation, idempotency_key, status, token, result, lease_expires_at, expires_at)
VALUES (sqlc.arg(merchant_id)::uuid, 'hosted_checkout_creation', sqlc.arg(order_id)::uuid::text,
    'succeeded', sqlc.arg(order_id)::uuid, jsonb_build_object('secret', sqlc.arg(secret)::text),
    sqlc.arg(now)::timestamptz, sqlc.arg(expires_at)::timestamptz);

-- name: SetMyOrderCheckout :execrows
-- A customer's own open order takes its embedded checkout, in place of the
-- one it had: the last secret stops working. It lasts as long as the order
-- and returns nowhere.
UPDATE billing.orders
SET checkout_secret_hash = sqlc.arg(secret_hash)::bytea,
    checkout_expires_at = expires_at,
    checkout_success_url = NULL,
    checkout_cancel_url = NULL,
    checkout_saved_payment_methods = true,
    checkout_ui_mode = 'embedded',
    updated_at = sqlc.arg(now)::timestamptz
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid AND customer_id = sqlc.arg(customer_id)::uuid
  AND origin = 'customer' AND status = 'open' AND collection_owner = 'engine'
  AND expires_at > sqlc.arg(now)::timestamptz;
