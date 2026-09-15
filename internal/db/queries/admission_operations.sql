-- name: GetAdmissionOperation :one
SELECT * FROM openrails.admission_operations
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND request_id = sqlc.arg(request_id)::text;

-- name: LockAdmissionOperation :one
SELECT * FROM openrails.admission_operations
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND request_id = sqlc.arg(request_id)::text
FOR UPDATE;

-- name: InsertAdmissionOperation :one
INSERT INTO openrails.admission_operations (
    merchant_id, request_id, payer_id, currency, estimated_amount, available_amount, terms,
    requested_expires_at, expires_at, admitted_at, window_keys
) VALUES (
    sqlc.arg(merchant_id)::uuid, sqlc.arg(request_id)::text, sqlc.arg(payer_id)::uuid,
    sqlc.arg(currency)::text, sqlc.arg(estimated_amount)::bigint, sqlc.arg(available_amount)::bigint, sqlc.arg(terms)::jsonb,
    sqlc.narg(requested_expires_at)::timestamptz, sqlc.narg(requested_expires_at)::timestamptz,
    sqlc.arg(admitted_at)::timestamptz, sqlc.arg(window_keys)::text[]
)
ON CONFLICT (merchant_id, request_id) DO NOTHING
RETURNING *;

-- name: GetFinancialHeldAmount :one
SELECT openrails.financial_held_amount(sqlc.arg(merchant_id)::uuid, sqlc.arg(payer_id)::uuid,
    sqlc.arg(currency)::text, sqlc.arg(as_of)::timestamptz)::bigint AS held;

-- name: AdmissionWindowUsage :one
SELECT
    COALESCE(SUM(CASE WHEN state = 'captured' THEN captured_amount ELSE estimated_amount END), 0)::bigint AS used,
    COALESCE(SUM(CASE WHEN state = 'open' AND (expires_at IS NULL OR expires_at > sqlc.arg(as_of)::timestamptz)
        THEN estimated_amount ELSE 0 END), 0)::bigint AS reserved
FROM openrails.admission_operations
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND payer_id = sqlc.arg(payer_id)::uuid
  AND currency = sqlc.arg(currency)::text AND state <> 'released'
  AND admitted_at >= sqlc.arg(window_start)::timestamptz AND admitted_at < sqlc.arg(window_end)::timestamptz
  AND window_keys @> ARRAY[sqlc.arg(window_key)::text];

-- name: AdmissionCaptureTermsMatch :one
SELECT capture_terms = sqlc.arg(capture_terms)::jsonb AS matches
FROM openrails.admission_operations
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND request_id = sqlc.arg(request_id)::text;

-- name: CaptureAdmissionOperation :one
UPDATE openrails.admission_operations
SET state = 'captured', capture_terms = sqlc.arg(capture_terms)::jsonb, captured_amount = sqlc.arg(amount)::bigint, captured_at = sqlc.arg(as_of)::timestamptz
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND request_id = sqlc.arg(request_id)::text
  AND state <> 'captured'
RETURNING *;

-- name: ReleaseAdmissionOperation :execrows
UPDATE openrails.admission_operations
SET state = 'released', released_at = sqlc.arg(as_of)::timestamptz
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND request_id = sqlc.arg(request_id)::text AND state = 'open';

-- name: ExtendAdmissionOperation :execrows
UPDATE openrails.admission_operations SET expires_at = sqlc.arg(expires_at)::timestamptz
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND request_id = sqlc.arg(request_id)::text AND state = 'open'
  AND (expires_at IS NULL OR expires_at > sqlc.arg(as_of)::timestamptz)
  AND (expires_at IS NULL OR expires_at <= sqlc.arg(expires_at)::timestamptz);
