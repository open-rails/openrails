-- openrails.customers: payable-subject resolution (#317).

-- name: UpsertSelfCustomer :one
-- Self-service identities are their own UUID: materialize the row WITH
-- id = the subject UUID and return it unchanged (refreshing last_seen_at).
INSERT INTO openrails.customers (id, merchant_id, issuer, subject)
VALUES ($1, $2, $3, $4)
ON CONFLICT (id) DO UPDATE SET last_seen_at = now()
RETURNING id;

-- name: UpsertFederatedCustomer :one
-- Federated (delegated/tenant-issued) external subject: keyed by
-- (tenant, issuer, subject) with a generated id.
INSERT INTO openrails.customers (merchant_id, issuer, subject)
VALUES ($1, $2, $3)
ON CONFLICT (merchant_id, issuer, subject) DO UPDATE SET last_seen_at = now()
RETURNING id;

-- name: EnsureCustomerRow :exec
-- FK-target materialization before commerce inserts; no-op when present.
INSERT INTO openrails.customers (id, merchant_id, issuer, subject)
VALUES ($1, $2, $3, $4)
ON CONFLICT DO NOTHING;
