-- The copy of the merchant's directory: what SCIM pushed and verified tokens
-- claimed, newest first by directory_updated_at.

-- name: ListCustomerContacts :many
SELECT * FROM billing.customer_contacts
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND customer_id = ANY(sqlc.arg(customer_ids)::uuid[]);

-- name: SearchCustomerContacts :many
-- pattern is the query escaped for LIKE and wrapped in %; exact matches of
-- the username or email come first.
SELECT * FROM billing.customer_contacts c
WHERE c.merchant_id = sqlc.arg(merchant_id)::uuid
  AND (c.email ILIKE sqlc.arg(pattern)::text OR c.user_name ILIKE sqlc.arg(pattern)::text OR c.display_name ILIKE sqlc.arg(pattern)::text)
ORDER BY (lower(c.user_name) = lower(sqlc.arg(query)::text)) DESC NULLS LAST, (lower(c.email) = lower(sqlc.arg(query)::text)) DESC NULLS LAST, c.customer_id
LIMIT sqlc.arg(row_limit)::int;

-- name: GetCustomerContactForUpdate :one
SELECT * FROM billing.customer_contacts
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND customer_id = sqlc.arg(customer_id)::uuid
FOR UPDATE;

-- name: GetProvisionedContact :one
SELECT * FROM billing.customer_contacts
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND customer_id = sqlc.arg(customer_id)::uuid AND provisioned_at IS NOT NULL;

-- name: ListProvisionedContacts :many
-- A SCIM list: the Users a SCIM client holds, oldest first, by at most one
-- equality filter.
SELECT * FROM billing.customer_contacts c
WHERE c.merchant_id = sqlc.arg(merchant_id)::uuid AND c.provisioned_at IS NOT NULL
  AND (sqlc.narg(customer_id)::uuid IS NULL OR c.customer_id = sqlc.narg(customer_id)::uuid)
  AND (sqlc.narg(user_name)::text IS NULL OR lower(c.user_name) = lower(sqlc.narg(user_name)::text))
  AND (sqlc.narg(email)::text IS NULL OR lower(c.email) = lower(sqlc.narg(email)::text))
ORDER BY c.provisioned_at, c.customer_id
OFFSET sqlc.arg(row_offset)::int
LIMIT sqlc.arg(row_limit)::int;

-- name: CountProvisionedContacts :one
SELECT count(*) FROM billing.customer_contacts c
WHERE c.merchant_id = sqlc.arg(merchant_id)::uuid AND c.provisioned_at IS NOT NULL
  AND (sqlc.narg(customer_id)::uuid IS NULL OR c.customer_id = sqlc.narg(customer_id)::uuid)
  AND (sqlc.narg(user_name)::text IS NULL OR lower(c.user_name) = lower(sqlc.narg(user_name)::text))
  AND (sqlc.narg(email)::text IS NULL OR lower(c.email) = lower(sqlc.narg(email)::text));

-- name: InsertCustomerContact :one
INSERT INTO billing.customer_contacts (merchant_id, customer_id, email, display_name, user_name, active, provisioned_at, directory_updated_at)
VALUES (sqlc.arg(merchant_id)::uuid, sqlc.arg(customer_id)::uuid, sqlc.narg(email), sqlc.narg(display_name), sqlc.narg(user_name),
        sqlc.narg(active), sqlc.narg(provisioned_at), sqlc.narg(directory_updated_at))
RETURNING *;

-- name: UpdateCustomerContact :one
UPDATE billing.customer_contacts SET
  email = sqlc.narg(email), display_name = sqlc.narg(display_name), user_name = sqlc.narg(user_name),
  active = sqlc.narg(active), provisioned_at = sqlc.narg(provisioned_at),
  directory_updated_at = sqlc.narg(directory_updated_at), updated_at = now()
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND customer_id = sqlc.arg(customer_id)::uuid
RETURNING *;

-- name: RecordContactClaims :exec
-- A verified token's claims, in one statement that writes nothing when
-- nothing changed: a new contact takes them; a known one only when they are
-- dated and newer than what it holds. An absent claim keeps its value.
INSERT INTO billing.customer_contacts AS c (merchant_id, customer_id, email, display_name, user_name, directory_updated_at)
VALUES (sqlc.arg(merchant_id)::uuid, sqlc.arg(customer_id)::uuid, sqlc.narg(email), sqlc.narg(display_name), sqlc.narg(user_name), sqlc.narg(claims_updated_at))
ON CONFLICT (merchant_id, customer_id) DO UPDATE SET
  email = COALESCE(EXCLUDED.email, c.email), display_name = COALESCE(EXCLUDED.display_name, c.display_name),
  user_name = COALESCE(EXCLUDED.user_name, c.user_name), directory_updated_at = EXCLUDED.directory_updated_at, updated_at = now()
WHERE EXCLUDED.directory_updated_at IS NOT NULL
  AND (c.directory_updated_at IS NULL OR c.directory_updated_at < EXCLUDED.directory_updated_at)
  AND (c.email, c.display_name, c.user_name) IS DISTINCT FROM
      (COALESCE(EXCLUDED.email, c.email), COALESCE(EXCLUDED.display_name, c.display_name), COALESCE(EXCLUDED.user_name, c.user_name));
