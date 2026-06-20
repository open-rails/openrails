-- openrails.customers: payable balance account (#491). A customer is a PURE
-- balance keyed by its UUID id (#364); the caller/merchant supplies the id.

-- name: EnsureCustomer :one
-- Materialize (or refresh) the customers row for a payable UUID id under a
-- merchant. The caller supplies id (the payable UUID). ON CONFLICT refreshes
-- last_seen_at so concurrent first-touch is safe.
INSERT INTO openrails.customers (id, merchant_id, subject)
VALUES (sqlc.arg(id), sqlc.arg(merchant_id), sqlc.arg(subject))
ON CONFLICT (id) DO UPDATE SET
  subject = EXCLUDED.subject,
  last_seen_at = now()
RETURNING id;

-- name: EnsureCustomerRow :exec
-- FK-target materialization before commerce inserts; no-op when present.
INSERT INTO openrails.customers (id, merchant_id, subject)
VALUES (sqlc.arg(id), sqlc.arg(merchant_id), sqlc.arg(subject))
ON CONFLICT DO NOTHING;

-- name: LookupCustomerIDsBySubjects :many
-- Resolve merchant-local stable host subjects to customer ids. Issuer is audit
-- metadata only and never participates in identity.
SELECT id, subject FROM openrails.customers
WHERE merchant_id = $1
  AND subject = ANY(sqlc.arg(subjects)::text[]);

-- name: UpsertCustomerBySubject :one
-- Customer identity is the merchant plus the host/AuthKit stable UUID subject.
-- The row id is that subject UUID; issuer is kept only as last-seen audit source.
INSERT INTO openrails.customers (id, merchant_id, issuer, subject)
VALUES (sqlc.arg(subject)::uuid, sqlc.arg(merchant_id), sqlc.narg(issuer), sqlc.arg(subject))
ON CONFLICT (id) DO UPDATE SET
  subject = EXCLUDED.subject,
  issuer = COALESCE(EXCLUDED.issuer, openrails.customers.issuer),
  last_seen_at = now()
RETURNING id;
