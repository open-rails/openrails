-- Direct, read-only inspection of the existing durable admission
-- records. Each query bounds its partition key and binds its merchant.

-- Admitted requests: request holds.
-- name: ListRequestHolds :many
SELECT * FROM billing.limit_holds
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND request_id IS NOT NULL
  AND created_at >= sqlc.arg(from_at)::timestamptz
  AND created_at < sqlc.arg(to_at)::timestamptz
  AND (sqlc.narg(customer_id)::uuid IS NULL OR customer_id = sqlc.narg(customer_id)::uuid)
  AND (sqlc.narg(currency)::text IS NULL OR currency = sqlc.narg(currency)::text)
  AND (sqlc.narg(state)::text IS NULL OR
       CASE WHEN state = 'open' AND expires_at <= sqlc.arg(as_of)::timestamptz THEN 'expired' ELSE state END = sqlc.narg(state)::text)
  AND (sqlc.narg(after_at)::timestamptz IS NULL OR
       (created_at, request_id COLLATE "C") < (sqlc.narg(after_at)::timestamptz, sqlc.narg(after_request_id)::text COLLATE "C"))
ORDER BY created_at DESC, request_id COLLATE "C" DESC
LIMIT sqlc.arg(row_limit)::int;

