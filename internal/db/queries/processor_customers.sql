-- openrails.processor_customers: customer <-> processor customer mapping.

-- name: UpsertProcessorCustomer :exec
INSERT INTO openrails.processor_customers (
    id, merchant_id, customer_id, processor, processor_customer_id, created_at, updated_at
) VALUES ($1, sqlc.arg(merchant_id)::uuid, $2, $3, $4, $5, $6)
ON CONFLICT (merchant_id, customer_id, processor) WHERE provider_account_id IS NULL DO UPDATE SET
    processor_customer_id = EXCLUDED.processor_customer_id,
    updated_at = EXCLUDED.updated_at;

-- name: GetProcessorCustomerID :one
SELECT processor_customer_id FROM openrails.processor_customers
WHERE customer_id = $1 AND processor = $2
LIMIT 1;

-- name: GetProcessorCustomerIDForMerchant :one
SELECT processor_customer_id FROM openrails.processor_customers
WHERE merchant_id = $1 AND customer_id = $2 AND processor = $3
LIMIT 1;

-- name: GetProcessorCustomerSubject :one
SELECT customer_id::text FROM openrails.processor_customers
WHERE processor_customer_id = $1 AND processor = $2
ORDER BY updated_at DESC
LIMIT 1;
