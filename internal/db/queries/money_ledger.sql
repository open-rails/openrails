-- modules/money serialization: balance comes from the ledger (ledger.sql),
-- credit lots are grants (grants.sql), and the per-customer spend mutex is a
-- FOR UPDATE lock on the customers row.

-- name: LockCustomerForSpend :one
-- The per-customer spend mutex: every spend, capture and deposit path locks this
-- row FOR UPDATE before touching the customer's ledger. Returns the customer id.
SELECT id FROM billing.customers
WHERE id = $1 AND merchant_id = $2
FOR UPDATE;
