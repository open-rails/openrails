-- Durable request and webhook-delivery claims (#1099). Every statement names
-- the merchant; claims run on the base pool, independent of any caller tx.

-- name: ClaimIdempotencyKey :one
INSERT INTO openrails.idempotency_keys (merchant_id, operation, idempotency_key, status, lease_expires_at, expires_at, created_at, updated_at)
VALUES (sqlc.arg(merchant_id)::uuid, sqlc.arg(operation)::text, sqlc.arg(idempotency_key)::text, 'processing',
        sqlc.arg(lease_expires_at)::timestamptz, sqlc.arg(expires_at)::timestamptz, sqlc.arg(now)::timestamptz, sqlc.arg(now)::timestamptz)
ON CONFLICT (merchant_id, operation, idempotency_key) DO NOTHING
RETURNING *;

-- A failed, expired or lapsed-lease claim passes to exactly one caller: the
-- row lock serializes contenders and the loser re-evaluates the predicate
-- against the winner's row.
-- name: ReclaimIdempotencyKey :one
UPDATE openrails.idempotency_keys
SET status = 'processing', claims = claims + 1, result = NULL, error = NULL,
    lease_expires_at = sqlc.arg(lease_expires_at)::timestamptz,
    expires_at = sqlc.arg(expires_at)::timestamptz,
    updated_at = sqlc.arg(now)::timestamptz
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND operation = sqlc.arg(operation)::text
  AND idempotency_key = sqlc.arg(idempotency_key)::text
  AND (expires_at <= sqlc.arg(now)::timestamptz
       OR status = 'failed'
       OR (status = 'processing' AND lease_expires_at <= sqlc.arg(now)::timestamptz))
RETURNING *;

-- name: GetIdempotencyKey :one
SELECT * FROM openrails.idempotency_keys
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND operation = sqlc.arg(operation)::text
  AND idempotency_key = sqlc.arg(idempotency_key)::text;

-- name: RenewIdempotencyKey :execrows
UPDATE openrails.idempotency_keys
SET lease_expires_at = sqlc.arg(lease_expires_at)::timestamptz,
    expires_at = GREATEST(expires_at, sqlc.arg(lease_expires_at)::timestamptz),
    updated_at = sqlc.arg(now)::timestamptz
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND operation = sqlc.arg(operation)::text
  AND idempotency_key = sqlc.arg(idempotency_key)::text
  AND status = 'processing' AND claims = sqlc.arg(claims)::bigint;

-- name: CompleteIdempotencyKey :execrows
UPDATE openrails.idempotency_keys
SET status = 'succeeded', result = sqlc.narg(result)::jsonb,
    lease_expires_at = sqlc.arg(now)::timestamptz,
    expires_at = sqlc.arg(expires_at)::timestamptz,
    updated_at = sqlc.arg(now)::timestamptz
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND operation = sqlc.arg(operation)::text
  AND idempotency_key = sqlc.arg(idempotency_key)::text
  AND status = 'processing' AND claims = sqlc.arg(claims)::bigint;

-- name: FailIdempotencyKey :execrows
UPDATE openrails.idempotency_keys
SET status = 'failed', error = sqlc.arg(error)::text,
    lease_expires_at = sqlc.arg(now)::timestamptz,
    expires_at = sqlc.arg(expires_at)::timestamptz,
    updated_at = sqlc.arg(now)::timestamptz
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND operation = sqlc.arg(operation)::text
  AND idempotency_key = sqlc.arg(idempotency_key)::text
  AND status = 'processing' AND claims = sqlc.arg(claims)::bigint;

-- Bounded: row_limit caps one statement, the GC worker loops. SKIP LOCKED
-- leaves a row being reclaimed to its claimant; the outer predicate re-checks
-- expiry against the row actually deleted.
-- name: DeleteExpiredIdempotencyKeys :execrows
DELETE FROM openrails.idempotency_keys ik
USING (
    SELECT merchant_id, operation, idempotency_key FROM openrails.idempotency_keys
    WHERE expires_at <= sqlc.arg(now)::timestamptz
    ORDER BY expires_at
    LIMIT sqlc.arg(row_limit)::int
    FOR UPDATE SKIP LOCKED
) expired
WHERE ik.merchant_id = expired.merchant_id
  AND ik.operation = expired.operation
  AND ik.idempotency_key = expired.idempotency_key
  AND ik.expires_at <= sqlc.arg(now)::timestamptz;
