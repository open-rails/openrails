-- Customer links: an openrails.dev account linked to one merchant's customer.
-- Every query names merchant_id (the merchant's side) or account_id (the
-- network's side).

-- name: GetActiveCustomerLinkByCustomer :one
SELECT * FROM billing.customer_links
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND customer_id = sqlc.arg(customer_id)::uuid AND status = 'active'
FOR UPDATE;

-- name: GetActiveCustomerLinkByAccount :one
SELECT * FROM billing.customer_links
WHERE account_id = sqlc.arg(account_id)::uuid AND merchant_id = sqlc.arg(merchant_id)::uuid AND status = 'active';

-- name: InsertCustomerLink :one
INSERT INTO billing.customer_links
    (merchant_id, customer_id, account_id, method, credential, order_id, auth_time, amr, terms_version, status, linked_at)
VALUES (sqlc.arg(merchant_id)::uuid, sqlc.arg(customer_id)::uuid, sqlc.arg(account_id)::uuid, sqlc.arg(method)::text,
    sqlc.narg(credential)::text, sqlc.narg(order_id)::uuid, sqlc.narg(auth_time)::timestamptz, sqlc.narg(amr)::text[],
    sqlc.arg(terms_version)::text, 'active', sqlc.arg(linked_at)::timestamptz)
RETURNING *;

-- name: RevokeCustomerLink :one
UPDATE billing.customer_links
SET status = 'revoked', revoked_at = sqlc.arg(revoked_at)::timestamptz, revoked_by = sqlc.arg(revoked_by)::text
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid AND status = 'active'
RETURNING *;

-- name: ListActiveCustomerLinksByCustomers :many
-- The links of a page of the merchant's customers: at most one each.
SELECT * FROM billing.customer_links
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND customer_id = ANY(sqlc.arg(customer_ids)::uuid[]) AND status = 'active'
LIMIT cardinality(sqlc.arg(customer_ids)::uuid[]);

-- name: ListActiveCustomerLinksByAccount :many
-- The account's links, one per merchant, by merchant after a cursor.
SELECT * FROM billing.customer_links
WHERE account_id = sqlc.arg(account_id)::uuid AND status = 'active'
  AND (sqlc.narg(after_merchant)::uuid IS NULL OR merchant_id > sqlc.narg(after_merchant)::uuid)
ORDER BY merchant_id
LIMIT sqlc.arg(row_limit)::int;

-- name: GetCustomerLinkOfAccount :one
-- One of the account's links, active or ended.
SELECT * FROM billing.customer_links
WHERE account_id = sqlc.arg(account_id)::uuid AND id = sqlc.arg(id)::uuid;

-- name: GetCustomerLink :one
SELECT * FROM billing.customer_links
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid;

-- name: ListActiveCustomerLinksOfMerchant :many
-- A batch of the merchant's links, for revoking them all when it leaves.
SELECT * FROM billing.customer_links
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND status = 'active'
ORDER BY customer_id
LIMIT sqlc.arg(row_limit)::int;

-- name: CustomerHasLiveSubscriptions :one
-- A subscription not canceled still charges the customer's cards, or will.
SELECT EXISTS (
    SELECT 1 FROM billing.subscriptions
    WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND customer_id = sqlc.arg(customer_id)::uuid
      AND status <> 'canceled' AND deleted_at IS NULL
)::boolean AS live;

-- name: NativeCustomerExists :one
SELECT EXISTS (
    SELECT 1 FROM billing.customers
    WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid AND issuer IS NULL
)::boolean AS native;

-- name: DeleteRevokedCustomerLinksBefore :execrows
-- Batched: row_limit bounds one statement, the caller loops.
DELETE FROM billing.customer_links
WHERE ctid IN (
    SELECT l.ctid FROM billing.customer_links l
    WHERE l.merchant_id = sqlc.arg(merchant_id)::uuid AND l.status = 'revoked' AND l.revoked_at < sqlc.arg(cutoff)::timestamptz
    LIMIT sqlc.arg(row_limit)::int
);

-- name: EnqueueCustomerNetworkEvent :exec
-- One link transition: linking or unlinking the same link again collapses.
INSERT INTO billing.host_outbox (merchant_id, event_type, subject_type, subject_id, occurred_at, data, dedupe_key)
VALUES (sqlc.arg(merchant_id)::uuid, sqlc.arg(event_type)::text, 'customer', sqlc.arg(customer_id)::uuid,
    sqlc.arg(occurred_at)::timestamptz, sqlc.arg(data)::jsonb, sqlc.arg(event_type)::text || ':' || sqlc.arg(link_id)::text)
ON CONFLICT (merchant_id, dedupe_key) DO NOTHING;
