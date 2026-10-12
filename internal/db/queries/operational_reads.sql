-- Direct, read-only inspection of the existing durable admission and usage
-- records. Each query bounds its partition key and binds its merchant.

-- name: ListAdmissionOperations :many
SELECT * FROM billing.admission_operations
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND admitted_at >= sqlc.arg(from_at)::timestamptz
  AND admitted_at < sqlc.arg(to_at)::timestamptz
  AND (sqlc.narg(customer_id)::uuid IS NULL OR customer_id = sqlc.narg(customer_id)::uuid)
  AND (sqlc.narg(currency)::text IS NULL OR currency = sqlc.narg(currency)::text)
  AND (sqlc.narg(state)::text IS NULL OR
       CASE WHEN state = 'open' AND expires_at <= sqlc.arg(as_of)::timestamptz THEN 'expired' ELSE state END = sqlc.narg(state)::text)
  AND (sqlc.narg(after_at)::timestamptz IS NULL OR
       (admitted_at, request_id COLLATE "C") < (sqlc.narg(after_at)::timestamptz, sqlc.narg(after_request_id)::text COLLATE "C"))
ORDER BY admitted_at DESC, request_id COLLATE "C" DESC
LIMIT sqlc.arg(row_limit)::int;

-- name: GetUsageEventByID :one
SELECT * FROM billing.usage_events
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid
  AND occurred_at >= sqlc.arg(from_at)::timestamptz
  AND occurred_at <= sqlc.arg(to_at)::timestamptz
ORDER BY occurred_at DESC
LIMIT 1;

-- name: ListUsageEvents :many
SELECT * FROM billing.usage_events
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND occurred_at >= sqlc.arg(from_at)::timestamptz
  AND occurred_at < sqlc.arg(to_at)::timestamptz
  AND (sqlc.narg(customer_id)::uuid IS NULL OR customer_id = sqlc.narg(customer_id)::uuid)
  AND (sqlc.narg(currency)::text IS NULL OR currency = sqlc.narg(currency)::text)
  AND (sqlc.narg(event_type)::text IS NULL OR event_type = sqlc.narg(event_type)::text)
  AND (sqlc.narg(source)::text IS NULL OR source = sqlc.narg(source)::text)
  AND (sqlc.narg(source_id)::text IS NULL OR source_id = sqlc.narg(source_id)::text)
  AND (sqlc.narg(outcome)::text IS NULL OR outcome = sqlc.narg(outcome)::text)
  AND (sqlc.narg(after_at)::timestamptz IS NULL OR
       (occurred_at, id) < (sqlc.narg(after_at)::timestamptz, sqlc.narg(after_id)::uuid))
ORDER BY occurred_at DESC, id DESC
LIMIT sqlc.arg(row_limit)::int;
