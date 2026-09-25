-- Durable request and webhook-delivery claims (#1099). Every statement names
-- the merchant, and every time is the database's now(): replicas' clocks
-- never decide who owns a key.

-- name: ClaimIdempotencyKey :one
INSERT INTO openrails.idempotency_keys (merchant_id, operation, idempotency_key, status, token, lease_expires_at, expires_at)
VALUES (sqlc.arg(merchant_id)::uuid, sqlc.arg(operation)::text, sqlc.arg(idempotency_key)::text, 'processing', sqlc.arg(token)::uuid,
        now() + make_interval(secs => sqlc.arg(lease_seconds)::float8),
        now() + make_interval(secs => sqlc.arg(ttl_seconds)::float8))
ON CONFLICT (merchant_id, operation, idempotency_key) DO NOTHING
RETURNING *;

-- A failed, expired or lapsed-lease claim passes to exactly one caller: the
-- row lock serializes contenders and the loser re-evaluates the predicate
-- against the winner's row.
-- name: ReclaimIdempotencyKey :one
UPDATE openrails.idempotency_keys
SET status = 'processing', token = sqlc.arg(token)::uuid, claims = claims + 1, result = NULL, error = NULL,
    lease_expires_at = now() + make_interval(secs => sqlc.arg(lease_seconds)::float8),
    expires_at = now() + make_interval(secs => sqlc.arg(ttl_seconds)::float8),
    updated_at = now()
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND operation = sqlc.arg(operation)::text
  AND idempotency_key = sqlc.arg(idempotency_key)::text
  AND (expires_at <= now()
       OR status = 'failed'
       OR (status = 'processing' AND lease_expires_at <= now()))
RETURNING *;

-- name: GetIdempotencyKey :one
SELECT *, (status = 'processing' AND lease_expires_at > now())::boolean AS leased
FROM openrails.idempotency_keys
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND operation = sqlc.arg(operation)::text
  AND idempotency_key = sqlc.arg(idempotency_key)::text;

-- name: RenewIdempotencyKey :execrows
UPDATE openrails.idempotency_keys
SET lease_expires_at = now() + make_interval(secs => sqlc.arg(lease_seconds)::float8),
    expires_at = GREATEST(expires_at, now() + make_interval(secs => sqlc.arg(lease_seconds)::float8)),
    updated_at = now()
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND operation = sqlc.arg(operation)::text
  AND idempotency_key = sqlc.arg(idempotency_key)::text
  AND status = 'processing' AND token = sqlc.arg(token)::uuid AND lease_expires_at > now();

-- The owner's own transaction proves it still holds the claim. The share
-- lock makes a reclaim wait for that transaction, so a superseded owner can
-- never commit.
-- name: HoldIdempotencyKeyInTx :one
SELECT true::boolean AS held FROM openrails.idempotency_keys
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND operation = sqlc.arg(operation)::text
  AND idempotency_key = sqlc.arg(idempotency_key)::text
  AND status = 'processing' AND token = sqlc.arg(token)::uuid AND lease_expires_at > now()
FOR SHARE;

-- name: CompleteIdempotencyKey :execrows
UPDATE openrails.idempotency_keys
SET status = 'succeeded', result = sqlc.narg(result)::jsonb,
    lease_expires_at = now(),
    expires_at = now() + make_interval(secs => sqlc.arg(ttl_seconds)::float8),
    updated_at = now()
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND operation = sqlc.arg(operation)::text
  AND idempotency_key = sqlc.arg(idempotency_key)::text
  AND status = 'processing' AND token = sqlc.arg(token)::uuid;

-- name: FailIdempotencyKey :execrows
UPDATE openrails.idempotency_keys
SET status = 'failed', error = sqlc.arg(error)::text,
    lease_expires_at = now(),
    expires_at = now() + make_interval(secs => sqlc.arg(ttl_seconds)::float8),
    updated_at = now()
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND operation = sqlc.arg(operation)::text
  AND idempotency_key = sqlc.arg(idempotency_key)::text
  AND status = 'processing' AND token = sqlc.arg(token)::uuid;

-- Bounded: row_limit caps one statement, the GC worker loops. SKIP LOCKED
-- leaves a row being reclaimed to its claimant; the outer predicate re-checks
-- expiry against the row actually deleted.
-- name: DeleteExpiredIdempotencyKeys :execrows
DELETE FROM openrails.idempotency_keys ik
USING (
    SELECT merchant_id, operation, idempotency_key FROM openrails.idempotency_keys
    WHERE expires_at <= now()
    ORDER BY expires_at
    LIMIT sqlc.arg(row_limit)::int
    FOR UPDATE SKIP LOCKED
) expired
WHERE ik.merchant_id = expired.merchant_id
  AND ik.operation = expired.operation
  AND ik.idempotency_key = expired.idempotency_key
  AND ik.expires_at <= now();
