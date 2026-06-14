-- openrails.processor_customers: payable-subject <-> processor customer mapping.

-- name: UpsertProcessorCustomer :exec
INSERT INTO openrails.processor_customers (
    id, merchant_id, merchant_subject_id, processor, customer_id, created_at, updated_at
) VALUES ($1, sqlc.arg(merchant_id)::uuid, $2, $3, $4, $5, $6)
ON CONFLICT (merchant_id, merchant_subject_id, processor) DO UPDATE SET
    customer_id = EXCLUDED.customer_id,
    updated_at = EXCLUDED.updated_at;

-- name: GetProcessorCustomerID :one
SELECT customer_id FROM openrails.processor_customers
WHERE merchant_subject_id = $1 AND processor = $2
LIMIT 1;

-- name: GetProcessorCustomerSubject :one
SELECT merchant_subject_id::text FROM openrails.processor_customers
WHERE customer_id = $1 AND processor = $2
ORDER BY updated_at DESC
LIMIT 1;
