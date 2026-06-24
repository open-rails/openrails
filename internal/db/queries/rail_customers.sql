-- openrails.rail_customers: customer <-> rail customer mapping.

-- name: UpsertRailCustomer :exec
INSERT INTO openrails.rail_customers (
    id, merchant_id, customer_id, rail, rail_customer_id, created_at, updated_at
) VALUES ($1, sqlc.arg(merchant_id)::uuid, $2, $3, $4, $5, $6)
ON CONFLICT (merchant_id, customer_id, rail) WHERE provider_account_id IS NULL DO UPDATE SET
    rail_customer_id = EXCLUDED.rail_customer_id,
    updated_at = EXCLUDED.updated_at;

-- name: GetRailCustomerID :one
SELECT rail_customer_id FROM openrails.rail_customers
WHERE customer_id = $1 AND rail = $2
LIMIT 1;

-- name: GetRailCustomerIDForMerchant :one
SELECT rail_customer_id FROM openrails.rail_customers
WHERE merchant_id = $1 AND customer_id = $2 AND rail = $3
LIMIT 1;

-- name: GetRailCustomerSubject :one
SELECT customer_id::text FROM openrails.rail_customers
WHERE rail_customer_id = $1 AND rail = $2
ORDER BY updated_at DESC
LIMIT 1;
