-- A payable identity is (merchant_id, issuer, id) (OIDC Core §5.7): id is the
-- issuer's stable UUID subject, issuer NULL for the host's own users. The same
-- person can have independent billing relationships with merchants.

-- name: EnsureCustomer :one
-- Refresh only the selected merchant's row. A new row is the host's own
-- user's (NULL issuer); an existing row keeps its issuer, which is identity.
INSERT INTO billing.customers (id, merchant_id)
VALUES (sqlc.arg(id), sqlc.arg(merchant_id))
ON CONFLICT (merchant_id, id) DO UPDATE SET
  last_seen_at = now()
RETURNING *;

-- name: CreateCustomer :exec
-- A customer's first authenticated request makes its row with the issuer of
-- its subject (NULL: the host's own users). An existing row is unchanged.
INSERT INTO billing.customers (id, merchant_id, issuer)
VALUES (sqlc.arg(id), sqlc.arg(merchant_id), sqlc.narg(issuer))
ON CONFLICT (merchant_id, id) DO NOTHING;

-- name: EnsureCustomerRow :exec
-- FK-target materialization before commerce writes. The scoped primary key
-- resolves concurrent first touches without a read or cross-merchant claim.
INSERT INTO billing.customers (id, merchant_id)
VALUES (sqlc.arg(id), sqlc.arg(merchant_id))
ON CONFLICT (merchant_id, id) DO NOTHING;

-- name: GetCustomer :one
SELECT * FROM billing.customers
WHERE merchant_id = sqlc.arg(merchant_id) AND id = sqlc.arg(id);

-- name: ListCustomersByIDs :many
SELECT * FROM billing.customers
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = ANY(sqlc.arg(ids)::uuid[])
ORDER BY created_at DESC, id DESC;

-- name: ListCustomers :many
-- Newest first.
SELECT * FROM billing.customers c
WHERE c.merchant_id = sqlc.arg(merchant_id)
  AND (sqlc.narg(after_at)::timestamptz IS NULL
   OR (c.created_at, c.id) < (sqlc.narg(after_at)::timestamptz, sqlc.narg(after_id)::uuid))
ORDER BY c.created_at DESC, c.id DESC
LIMIT sqlc.arg(row_limit)::int;

-- The hosted portal's "which merchants am I a customer of" directory, read
-- before any merchant is chosen: the host's own user's customers, never a
-- trusted issuer's with the same subject.
-- name: ListMerchantsForCustomerSubject :many
SELECT m.id, m.slug
FROM billing.merchants m
WHERE m.deleted_at IS NULL
  AND m.status = 'active'
  AND m.id IN (SELECT c.merchant_id FROM billing.customers c WHERE c.id = sqlc.arg(subject)::uuid AND c.issuer IS NULL)
ORDER BY m.slug;
