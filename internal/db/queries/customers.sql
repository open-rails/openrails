-- A payable identity is (merchant_id, id). The host supplies the stable UUID;
-- the same person can have independent billing relationships with merchants.

-- name: EnsureCustomer :one
-- Refresh only the selected merchant's row. Issuer is audit metadata and does
-- not participate in identity; callers without an issuer preserve its value.
INSERT INTO openrails.customers (id, merchant_id, issuer)
VALUES (sqlc.arg(id), sqlc.arg(merchant_id), sqlc.narg(issuer))
ON CONFLICT (merchant_id, id) DO UPDATE SET
  issuer = COALESCE(EXCLUDED.issuer, openrails.customers.issuer),
  last_seen_at = now()
RETURNING id;

-- name: EnsureCustomerRow :exec
-- FK-target materialization before commerce writes. The scoped primary key
-- resolves concurrent first touches without a read or cross-merchant claim.
INSERT INTO openrails.customers (id, merchant_id)
VALUES (sqlc.arg(id), sqlc.arg(merchant_id))
ON CONFLICT (merchant_id, id) DO NOTHING;

-- name: SearchCustomers :many
-- Merchant-scoped customer list/search (#740). merchant_id is an EXPLICIT
-- predicate (defense-in-depth doctrine, #227): RLS still pins the merchant on
-- enforcing roles, but a BYPASSRLS role (development's owner connection) must
-- never see another merchant's customers. q matches the subject UUID
-- prefix or a subscription email substring; empty q lists
-- newest-touched first. email is the latest subscription email on file
-- (customers carry none themselves).
SELECT c.id, c.id::text AS subject, c.created_at, c.last_seen_at,
  (SELECT s.user_email FROM openrails.subscriptions s
     WHERE s.customer_id = c.id AND s.merchant_id = c.merchant_id
       AND s.deleted_at IS NULL
       AND s.user_email IS NOT NULL
     ORDER BY s.created_at DESC LIMIT 1) AS email
FROM openrails.customers c
WHERE c.merchant_id = sqlc.arg(merchant_id)
  AND (sqlc.arg(q)::text = ''
   OR c.id::text ILIKE sqlc.arg(q) || '%'
   OR EXISTS (
        SELECT 1 FROM openrails.subscriptions se
        WHERE se.customer_id = c.id
          AND se.merchant_id = c.merchant_id
          AND se.deleted_at IS NULL
          AND se.user_email ILIKE '%' || sqlc.arg(q) || '%'))
ORDER BY c.last_seen_at DESC
LIMIT sqlc.arg(page_limit) OFFSET sqlc.arg(page_offset);

-- name: CountSearchCustomers :one
SELECT count(*) FROM openrails.customers c
WHERE c.merchant_id = sqlc.arg(merchant_id)
  AND (sqlc.arg(q)::text = ''
   OR c.id::text ILIKE sqlc.arg(q) || '%'
   OR EXISTS (
        SELECT 1 FROM openrails.subscriptions se
        WHERE se.customer_id = c.id
          AND se.merchant_id = c.merchant_id
          AND se.deleted_at IS NULL
          AND se.user_email ILIKE '%' || sqlc.arg(q) || '%'));

-- name: GetLatestCustomerEmail :one
-- Customers do not own an email column. Project the latest non-empty email from
-- all subscription history so an inactive customer remains identifiable on the
-- detail page. The explicit merchant predicate protects BYPASSRLS connections.
SELECT COALESCE((
  SELECT BTRIM(s.user_email)
  FROM openrails.subscriptions s
  WHERE s.customer_id = sqlc.arg(customer_id)
    AND s.merchant_id = sqlc.arg(merchant_id)
    AND s.deleted_at IS NULL
    AND NULLIF(BTRIM(s.user_email), '') IS NOT NULL
  ORDER BY s.created_at DESC, s.id DESC
  LIMIT 1
), '')::text AS email;

-- #824: the hosted portal's "which merchants am I a customer of" directory
-- (openrails-saas #18). openrails.merchants is global/policy-free, so only the
-- customers half needs the SECURITY DEFINER cross-merchant reader (0016).
-- name: ListMerchantsForCustomerSubject :many
SELECT m.id, m.slug, COALESCE(m.display_name, '')::text AS display_name
FROM openrails.merchants m
WHERE m.deleted_at IS NULL
  AND m.status = 'active'
  AND m.id IN (
      SELECT merchant_id FROM openrails.customer_merchant_ids_for_subject(sqlc.arg(subject)::uuid)
  )
ORDER BY m.slug;
