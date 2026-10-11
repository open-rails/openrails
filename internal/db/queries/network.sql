-- The customer network's reads across merchants: one query over the
-- account's active links, each merchant's rows joined on (merchant_id,
-- customer_id) and answered with the link they came through. Keyset cursor (created_at, merchant_id, id), newest first;
-- the work scales with the account's links, not with the merchants.

-- name: ListNetworkSubscriptionPage :many
SELECT l.id AS link_id, s.merchant_id, s.customer_id, s.id, s.created_at
FROM billing.customer_links l
CROSS JOIN LATERAL (
    SELECT sub.merchant_id, sub.customer_id, sub.id, sub.created_at FROM billing.subscriptions sub
    WHERE sub.merchant_id = l.merchant_id AND sub.customer_id = l.customer_id AND sub.deleted_at IS NULL
      AND (sqlc.narg(after_at)::timestamptz IS NULL
           OR (sub.created_at, sub.merchant_id, sub.id) < (sqlc.narg(after_at)::timestamptz, sqlc.narg(after_merchant)::uuid, sqlc.narg(after_id)::uuid))
    ORDER BY sub.created_at DESC, sub.id DESC
    LIMIT sqlc.arg(row_limit)::int
) s
WHERE l.account_id = sqlc.arg(account_id)::uuid AND l.status = 'active'
ORDER BY s.created_at DESC, s.merchant_id DESC, s.id DESC
LIMIT sqlc.arg(row_limit)::int;

-- name: ListNetworkOrderPage :many
SELECT l.id AS link_id, o.merchant_id, o.customer_id, o.id, o.created_at
FROM billing.customer_links l
CROSS JOIN LATERAL (
    SELECT ord.merchant_id, ord.customer_id, ord.id, ord.created_at FROM billing.orders ord
    WHERE ord.merchant_id = l.merchant_id AND ord.customer_id = l.customer_id
      AND (sqlc.narg(after_at)::timestamptz IS NULL
           OR (ord.created_at, ord.merchant_id, ord.id) < (sqlc.narg(after_at)::timestamptz, sqlc.narg(after_merchant)::uuid, sqlc.narg(after_id)::uuid))
    ORDER BY ord.created_at DESC, ord.id DESC
    LIMIT sqlc.arg(row_limit)::int
) o
WHERE l.account_id = sqlc.arg(account_id)::uuid AND l.status = 'active'
ORDER BY o.created_at DESC, o.merchant_id DESC, o.id DESC
LIMIT sqlc.arg(row_limit)::int;
