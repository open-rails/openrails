-- openrails.rail_customer_accounts: customer <-> rail customer mapping.

-- name: UpsertRailCustomerAccount :exec
INSERT INTO openrails.rail_customer_accounts (
    id, merchant_id, customer_id, rail, account_id, created_at, updated_at
) VALUES ($1, sqlc.arg(merchant_id)::uuid, $2, $3, $4, $5, $6)
ON CONFLICT (merchant_id, customer_id, rail) WHERE rail_merchant_account_id IS NULL DO UPDATE SET
    account_id = EXCLUDED.account_id,
    updated_at = EXCLUDED.updated_at;

-- name: GetRailCustomerAccountID :one
SELECT account_id FROM openrails.rail_customer_accounts
WHERE customer_id = $1 AND rail = $2
LIMIT 1;

-- name: GetRailCustomerAccountIDForMerchant :one
SELECT account_id FROM openrails.rail_customer_accounts
WHERE merchant_id = $1 AND customer_id = $2 AND rail = $3
LIMIT 1;

-- name: GetRailCustomerAccountSubject :one
SELECT customer_id::text FROM openrails.rail_customer_accounts
WHERE account_id = $1 AND rail = $2
ORDER BY updated_at DESC
LIMIT 1;
