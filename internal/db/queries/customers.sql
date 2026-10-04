-- A payable identity is (merchant_id, id). The host supplies the stable UUID;
-- the same person can have independent billing relationships with merchants.

-- name: EnsureCustomer :one
-- Refresh only the selected merchant's row. Issuer is audit metadata and does
-- not participate in identity; callers without an issuer preserve its value.
INSERT INTO billing.customers (id, merchant_id, issuer)
VALUES (sqlc.arg(id), sqlc.arg(merchant_id), sqlc.narg(issuer))
ON CONFLICT (merchant_id, id) DO UPDATE SET
  issuer = COALESCE(EXCLUDED.issuer, billing.customers.issuer),
  last_seen_at = now()
RETURNING *;

-- name: EnsureCustomerRow :exec
-- FK-target materialization before commerce writes. The scoped primary key
-- resolves concurrent first touches without a read or cross-merchant claim.
INSERT INTO billing.customers (id, merchant_id)
VALUES (sqlc.arg(id), sqlc.arg(merchant_id))
ON CONFLICT (merchant_id, id) DO NOTHING;

-- name: PutCustomer :one
-- The merchant's declaration of a customer: materialize it, or replace its
-- declared fields.
INSERT INTO billing.customers (id, merchant_id, email)
VALUES (sqlc.arg(id), sqlc.arg(merchant_id), sqlc.narg(email))
ON CONFLICT (merchant_id, id) DO UPDATE SET
  email = EXCLUDED.email,
  last_seen_at = now()
RETURNING *;

-- name: SetCustomerEmail :exec
UPDATE billing.customers SET email = sqlc.arg(email)
WHERE merchant_id = sqlc.arg(merchant_id) AND id = sqlc.arg(id);

-- name: GetCustomer :one
SELECT * FROM billing.customers
WHERE merchant_id = sqlc.arg(merchant_id) AND id = sqlc.arg(id);

-- name: ListCustomers :many
-- Newest first. q matches an id prefix or an email substring.
SELECT * FROM billing.customers c
WHERE c.merchant_id = sqlc.arg(merchant_id)
  AND (sqlc.arg(q)::text = ''
   OR c.id::text ILIKE sqlc.arg(q) || '%'
   OR c.email ILIKE '%' || sqlc.arg(q) || '%')
  AND (sqlc.narg(after_at)::timestamptz IS NULL
   OR (c.created_at, c.id) < (sqlc.narg(after_at)::timestamptz, sqlc.narg(after_id)::uuid))
ORDER BY c.created_at DESC, c.id DESC
LIMIT sqlc.arg(row_limit)::int;

-- The hosted portal's "which merchants am I a customer of" directory, read
-- before any merchant is chosen.
-- name: ListMerchantsForCustomerSubject :many
SELECT m.id, m.slug, COALESCE(m.display_name, '')::text AS display_name
FROM billing.merchants m
WHERE m.deleted_at IS NULL
  AND m.status = 'active'
  AND m.id IN (SELECT c.merchant_id FROM billing.customers c WHERE c.id = sqlc.arg(subject)::uuid)
ORDER BY m.slug;
