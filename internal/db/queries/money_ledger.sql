-- modules/money serialization. After the #512 hard cut the single-entry
-- money_blocks / money_transactions tables are gone: balance is derived from the
-- #512 ledger (internal/db/queries/ledger.sql), credit lots are #514 grants
-- (grants.sql), and the per-customer spend mutex is a FOR UPDATE lock on the
-- customers row. Admission holds live in Redis (#513), not Postgres.

-- name: LockCustomerForSpend :one
-- The per-customer spend mutex (#491): every spend/capture/deposit path locks
-- this row FOR UPDATE before reading/mutating the customer's ledger, serializing
-- all money mutations per customer. Returns the customer id.
SELECT id FROM openrails.customers
WHERE id = $1 AND merchant_id = $2
FOR UPDATE;
