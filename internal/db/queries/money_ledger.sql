-- modules/money serialization: balance comes from the ledger (ledger.sql),
-- credit lots are grants (grants.sql), and the per-customer spend mutex is a
-- FOR NO KEY UPDATE lock on the customers row.

-- name: LockCustomerForSpend :one
-- The per-customer spend mutex: every spend, capture and deposit path locks this
-- row before touching the customer's ledger. NO KEY: rows that only reference
-- the customer (its MRR) take KEY SHARE and never wait on it. Returns the
-- customer id.
SELECT id FROM billing.customers
WHERE id = $1 AND merchant_id = $2
FOR NO KEY UPDATE;
