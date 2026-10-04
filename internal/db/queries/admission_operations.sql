-- billing.admission_operations is partitioned by month on admitted_at. A lookup
-- by request id names the retained range; everything after it names the row's
-- own admitted_at, which is one partition.

-- Serializes admission per request id: the partitioned key carries admitted_at,
-- so it cannot refuse the same id admitted twice.
-- name: LockAdmissionRequest :exec
SELECT pg_advisory_xact_lock(hashtextextended('openrails.admission_request:' || sqlc.arg(merchant_id)::uuid::text || ':' || sqlc.arg(request_id)::text, 0));

-- name: GetAdmissionOperation :one
SELECT * FROM billing.admission_operations
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND request_id = sqlc.arg(request_id)::text
  AND admitted_at >= sqlc.arg(admitted_since)::timestamptz
ORDER BY admitted_at DESC
LIMIT 1;

-- name: LockAdmissionOperation :one
SELECT * FROM billing.admission_operations
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND request_id = sqlc.arg(request_id)::text
  AND admitted_at = sqlc.arg(admitted_at)::timestamptz
FOR UPDATE;

-- name: InsertAdmissionOperation :one
INSERT INTO billing.admission_operations (
    merchant_id, request_id, customer_id, currency, estimated_amount, available_amount, terms,
    requested_expires_at, expires_at, admitted_at, window_keys
) VALUES (
    sqlc.arg(merchant_id)::uuid, sqlc.arg(request_id)::text, sqlc.arg(customer_id)::uuid,
    sqlc.arg(currency)::text, sqlc.arg(estimated_amount)::bigint, sqlc.arg(available_amount)::bigint, sqlc.arg(terms)::jsonb,
    sqlc.narg(requested_expires_at)::timestamptz, sqlc.narg(requested_expires_at)::timestamptz,
    sqlc.arg(admitted_at)::timestamptz, sqlc.arg(window_keys)::text[]
)
RETURNING *;

-- name: GetFinancialHeldAmount :one
-- The one financial hold total: open operation authorizations plus open,
-- unexpired admission reservations. GetAdmissionCapacity computes the same sum.
-- A live hold was admitted within the hold lifetime, which is held_since.
SELECT (COALESCE((SELECT SUM(oa.amount)
              FROM billing.operation_authorizations oa
             WHERE oa.merchant_id = sqlc.arg(merchant_id)::uuid AND oa.customer_id = sqlc.arg(customer_id)::uuid AND oa.currency = sqlc.arg(currency)::text AND oa.state = 'open'), 0)
     + COALESCE((SELECT SUM(ao.estimated_amount)
              FROM billing.admission_operations ao
             WHERE ao.merchant_id = sqlc.arg(merchant_id)::uuid AND ao.customer_id = sqlc.arg(customer_id)::uuid AND ao.currency = sqlc.arg(currency)::text AND ao.state = 'open'
               AND ao.admitted_at >= sqlc.arg(held_since)::timestamptz
               AND ao.expires_at > sqlc.arg(as_of)::timestamptz), 0))::bigint AS held;

-- name: AdmissionWindowUsage :one
SELECT
    COALESCE(SUM(CASE WHEN state = 'captured' THEN captured_amount ELSE estimated_amount END), 0)::bigint AS used,
    COALESCE(SUM(CASE WHEN state = 'open' AND (expires_at IS NULL OR expires_at > sqlc.arg(as_of)::timestamptz)
        THEN estimated_amount ELSE 0 END), 0)::bigint AS reserved
FROM billing.admission_operations
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND customer_id = sqlc.arg(customer_id)::uuid
  AND currency = sqlc.arg(currency)::text AND state <> 'released'
  AND admitted_at >= sqlc.arg(window_start)::timestamptz AND admitted_at < sqlc.arg(window_end)::timestamptz
  AND window_keys @> ARRAY[sqlc.arg(window_key)::text];

-- name: AdmissionCaptureTermsMatch :one
SELECT capture_terms = sqlc.arg(capture_terms)::jsonb AS matches
FROM billing.admission_operations
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND request_id = sqlc.arg(request_id)::text
  AND admitted_at = sqlc.arg(admitted_at)::timestamptz;

-- name: CaptureAdmissionOperation :one
UPDATE billing.admission_operations
SET state = 'captured', capture_terms = sqlc.arg(capture_terms)::jsonb, captured_amount = sqlc.arg(amount)::bigint, captured_at = sqlc.arg(as_of)::timestamptz
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND request_id = sqlc.arg(request_id)::text
  AND admitted_at = sqlc.arg(admitted_at)::timestamptz
  AND state <> 'captured'
RETURNING *;

-- name: ReleaseAdmissionOperation :execrows
UPDATE billing.admission_operations
SET state = 'released', released_at = sqlc.arg(as_of)::timestamptz
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND request_id = sqlc.arg(request_id)::text
  AND admitted_at = sqlc.arg(admitted_at)::timestamptz AND state = 'open';

-- name: ExtendAdmissionOperation :execrows
UPDATE billing.admission_operations SET expires_at = sqlc.arg(expires_at)::timestamptz
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND request_id = sqlc.arg(request_id)::text
  AND admitted_at = sqlc.arg(admitted_at)::timestamptz AND state = 'open'
  AND (expires_at IS NULL OR expires_at > sqlc.arg(as_of)::timestamptz)
  AND (expires_at IS NULL OR expires_at <= sqlc.arg(expires_at)::timestamptz);
