-- billing.checkout_sessions (#1124). The row is immutable after mint
-- except for the payment attempt and the engine session it created. Times are
-- the engine clock's, passed in.

-- name: CreateCheckoutSession :execrows
INSERT INTO billing.checkout_sessions (
    merchant_id, id_hash, customer_id, price_id, offer, success_url, origin, expires_at, purge_at, created_at
) VALUES (
    sqlc.arg(merchant_id)::uuid, sqlc.arg(id_hash)::bytea, sqlc.arg(customer_id)::uuid, sqlc.arg(price_id)::uuid,
    sqlc.arg(offer)::jsonb, NULLIF(sqlc.arg(success_url)::text, ''), NULLIF(sqlc.arg(origin)::text, ''),
    sqlc.arg(expires_at)::timestamptz, sqlc.arg(purge_at)::timestamptz, sqlc.arg(now)::timestamptz
)
ON CONFLICT (merchant_id, id_hash) DO NOTHING;

-- name: GetCheckoutSession :one
SELECT customer_id, price_id, offer, success_url, origin, attempt, attempt_id, expires_at
FROM billing.checkout_sessions
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND id_hash = sqlc.arg(id_hash)::bytea
  AND purge_at > sqlc.arg(now)::timestamptz;

-- Records the engine session of an attempt that is still current and unbound.
-- name: BindCheckoutSessionAttempt :execrows
UPDATE billing.checkout_sessions
SET attempt_id = sqlc.arg(attempt_id)::uuid
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND id_hash = sqlc.arg(id_hash)::bytea
  AND attempt = sqlc.arg(attempt)::int
  AND attempt_id IS NULL;

-- Compare-and-set on attempt: concurrent callers advance a terminally failed
-- attempt once.
-- name: AdvanceCheckoutSessionAttempt :execrows
UPDATE billing.checkout_sessions
SET attempt = attempt + 1, attempt_id = NULL
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND id_hash = sqlc.arg(id_hash)::bytea
  AND attempt = sqlc.arg(attempt)::int
  AND purge_at > sqlc.arg(now)::timestamptz;

-- Bounded: row_limit caps one statement and the cleanup worker loops.
-- name: DeleteExpiredCheckoutSessions :execrows
DELETE FROM billing.checkout_sessions s
USING (
    SELECT merchant_id, id_hash FROM billing.checkout_sessions
    WHERE purge_at <= sqlc.arg(now)::timestamptz
    ORDER BY purge_at
    LIMIT sqlc.arg(row_limit)::int
    FOR UPDATE SKIP LOCKED
) expired
WHERE s.merchant_id = expired.merchant_id
  AND s.id_hash = expired.id_hash
  AND s.purge_at <= sqlc.arg(now)::timestamptz;

-- Credential directory: resolve only a complete opaque capability hash before
-- any merchant connection is pinned. Two rows reveal ambiguity, never a choice.
-- name: ResolveCheckoutSessionMerchant :many
SELECT s.merchant_id
FROM billing.checkout_sessions s
JOIN billing.merchants m ON m.id = s.merchant_id
WHERE s.id_hash = sqlc.arg(id_hash)::bytea
  AND s.purge_at > sqlc.arg(now)::timestamptz
  AND m.deleted_at IS NULL AND m.status = 'active'
LIMIT 2;
