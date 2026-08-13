-- openrails.customers: payable balance account (#491). A customer is a PURE
-- balance keyed by its UUID id (#364); the caller/merchant supplies the id.

-- name: EnsureCustomer :one
-- Materialize (or refresh) the customers row for a payable UUID id under a
-- merchant. The caller supplies id (the payable UUID). ON CONFLICT refreshes
-- last_seen_at so concurrent first-touch is safe. The merchant_id guard makes a
-- foreign id return NO ROW instead of re-pointing another merchant's customer
-- (#889) — RLS already blocks it on enforcing roles, this holds for the
-- privileged ones (bootstrap, import, dev owner) too.
INSERT INTO openrails.customers (id, merchant_id, subject)
VALUES (sqlc.arg(id), sqlc.arg(merchant_id), sqlc.arg(subject))
ON CONFLICT (id) DO UPDATE SET
  subject = EXCLUDED.subject,
  last_seen_at = now()
WHERE openrails.customers.merchant_id = EXCLUDED.merchant_id
RETURNING id;

-- name: EnsureCustomerRow :one
-- FK-target materialization before commerce inserts; no-op when present. It
-- RETURNS the id it materialized so a conflicting row owned by ANOTHER merchant
-- yields no row rather than a silent success (#889): FK checks bypass RLS, so a
-- silent no-op would let the caller's row attach to a foreign customer.
WITH inserted AS (
  INSERT INTO openrails.customers (id, merchant_id, subject)
  VALUES (sqlc.arg(id), sqlc.arg(merchant_id), sqlc.arg(subject))
  ON CONFLICT (id) DO NOTHING
  RETURNING id
)
SELECT id FROM inserted
UNION ALL
SELECT c.id FROM openrails.customers c
WHERE c.id = sqlc.arg(id) AND c.merchant_id = sqlc.arg(merchant_id)
LIMIT 1;

-- name: LockCustomerForMerchant :one
SELECT id FROM openrails.customers
WHERE id = sqlc.arg(id) AND merchant_id = sqlc.arg(merchant_id)
FOR UPDATE;

-- name: LookupCustomerIDsBySubjects :many
-- Resolve merchant-local stable host subjects to customer ids. Issuer is audit
-- metadata only and never participates in identity.
SELECT id, subject FROM openrails.customers
WHERE merchant_id = $1
  AND subject = ANY(sqlc.arg(subjects)::text[]);

-- name: SearchCustomers :many
-- Merchant-scoped customer list/search (#740). merchant_id is an EXPLICIT
-- predicate (defense-in-depth doctrine, #227): RLS still pins the merchant on
-- enforcing roles, but a BYPASSRLS role (development's owner connection) must
-- never see another merchant's customers. q matches subject (external ref)
-- prefix, id prefix, or a subscription email substring; empty q lists
-- newest-touched first. email is the latest subscription email on file
-- (customers carry none themselves).
SELECT c.id, c.subject, c.created_at, c.last_seen_at,
  (SELECT s.user_email FROM openrails.subscriptions s
     WHERE s.customer_id = c.id AND s.merchant_id = c.merchant_id
       AND s.deleted_at IS NULL
       AND s.user_email IS NOT NULL
     ORDER BY s.created_at DESC LIMIT 1) AS email
FROM openrails.customers c
WHERE c.merchant_id = sqlc.arg(merchant_id)
  AND (sqlc.arg(q)::text = ''
   OR c.subject ILIKE sqlc.arg(q) || '%'
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
   OR c.subject ILIKE sqlc.arg(q) || '%'
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

-- name: UpsertCustomerBySubject :one
-- Customer identity is the merchant plus the host/AuthKit stable UUID subject.
-- The row id is that subject UUID; issuer is kept only as last-seen audit source.
-- The merchant_id guard refuses a subject already registered under a DIFFERENT
-- merchant (#889) — one AuthKit instance can serve several merchants, and the
-- unguarded upsert handed the second merchant an id owned by the first.
INSERT INTO openrails.customers (id, merchant_id, issuer, subject)
VALUES (sqlc.arg(subject)::uuid, sqlc.arg(merchant_id), sqlc.narg(issuer), sqlc.arg(subject))
ON CONFLICT (id) DO UPDATE SET
  subject = EXCLUDED.subject,
  issuer = COALESCE(EXCLUDED.issuer, openrails.customers.issuer),
  last_seen_at = now()
WHERE openrails.customers.merchant_id = EXCLUDED.merchant_id
RETURNING id;

-- #824: the hosted portal's "which merchants am I a customer of" directory
-- (openrails-saas #18). openrails.merchants is global/policy-free, so only the
-- customers half needs the SECURITY DEFINER cross-merchant reader (0016).
-- name: ListMerchantsForCustomerSubject :many
SELECT m.slug, COALESCE(m.display_name, '')::text AS display_name
FROM openrails.merchants m
WHERE m.deleted_at IS NULL
  AND m.status = 'active'
  AND m.id IN (
      SELECT merchant_id FROM openrails.customer_merchant_ids_for_subject(sqlc.arg(subject)::text)
  )
ORDER BY m.slug;
