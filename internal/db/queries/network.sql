-- The customer network's reads across merchants: one query over the
-- account's active links, each merchant's rows joined on (merchant_id,
-- customer_id) and answered with the link they came through. Keyset cursor (created_at, merchant_id, id), newest first;
-- the work scales with the account's links, not with the merchants.

-- name: ListNetworkNotificationPage :many
SELECT l.id AS link_id, n.id, n.merchant_id, n.customer_id, n.event_type, n.data, n.read_at, n.created_at
FROM billing.customer_links l
CROSS JOIN LATERAL (
    SELECT notice.id, notice.merchant_id, notice.customer_id, notice.event_type, notice.data, notice.read_at, notice.created_at
    FROM billing.notifications notice
    WHERE notice.merchant_id = l.merchant_id AND notice.customer_id = l.customer_id
      AND notice.recipient_kind = 'customer'
      AND (sqlc.narg(seen)::boolean IS NULL OR (notice.read_at IS NOT NULL) = sqlc.narg(seen)::boolean)
      AND (sqlc.narg(after_at)::timestamptz IS NULL
           OR (notice.created_at, notice.merchant_id, notice.id) < (sqlc.narg(after_at)::timestamptz, sqlc.narg(after_merchant)::uuid, sqlc.narg(after_id)::uuid))
    ORDER BY notice.created_at DESC, notice.id DESC
    LIMIT sqlc.arg(row_limit)::int
) n
WHERE l.account_id = sqlc.arg(account_id)::uuid AND l.status = 'active'
ORDER BY n.created_at DESC, n.merchant_id DESC, n.id DESC
LIMIT sqlc.arg(row_limit)::int;

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
