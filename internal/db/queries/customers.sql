-- openrails.customers: payable balance account (#491). A customer is a PURE
-- balance keyed by its UUID id (#364); the caller/merchant supplies the id.

-- name: EnsureCustomer :one
-- Materialize (or refresh) the customers row for a payable UUID id under a
-- merchant. The caller supplies id (the payable UUID). ON CONFLICT refreshes
-- last_seen_at so concurrent first-touch is safe.
INSERT INTO openrails.customers (id, merchant_id)
VALUES ($1, $2)
ON CONFLICT (id) DO UPDATE SET last_seen_at = now()
RETURNING id;

-- name: EnsureCustomerRow :exec
-- FK-target materialization before commerce inserts; no-op when present.
INSERT INTO openrails.customers (id, merchant_id)
VALUES ($1, $2)
ON CONFLICT DO NOTHING;
