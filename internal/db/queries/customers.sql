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

-- name: UpsertCustomerByOrg :one
-- #491 org-bound payer: ONE customer per (merchant, org). uuidv7 pk minted once,
-- looked up by the natural key (no derived uuidv5). Returns the stable id.
INSERT INTO openrails.customers (merchant_id, org_id)
VALUES ($1, $2)
ON CONFLICT (merchant_id, org_id) WHERE org_id IS NOT NULL
DO UPDATE SET last_seen_at = now()
RETURNING id;

-- name: LookupCustomerIDsByIssuerSubjects :many
-- #491: resolve (merchant, issuer, subject) -> id for a batch of org-less
-- subjects. Replaces the dropped deterministic FederatedCustomerID derivation
-- with a real lookup against the re-added natural key. Absent subjects are simply
-- not returned.
SELECT id, subject FROM openrails.customers
WHERE merchant_id = $1 AND org_id IS NULL AND issuer = $2
  AND subject = ANY(sqlc.arg(subjects)::text[]);

-- name: UpsertCustomerByIssuerSubject :one
-- #491 org-less/standalone-federated payer: per (merchant, issuer, subject).
-- uuidv7 pk minted once, looked up by the natural key.
INSERT INTO openrails.customers (merchant_id, issuer, subject)
VALUES ($1, $2, $3)
ON CONFLICT (merchant_id, issuer, subject) WHERE org_id IS NULL AND issuer IS NOT NULL AND subject IS NOT NULL
DO UPDATE SET last_seen_at = now()
RETURNING id;
