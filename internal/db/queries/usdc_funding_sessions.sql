-- openrails.usdc_funding_sessions.

-- name: CreateUSDCFundingSession :exec
INSERT INTO openrails.usdc_funding_sessions (
    id, merchant_id, merchant_subject_id, checkout_session_id, provider,
    wallet_address, asset, network, requested_amount, provider_session_id,
    provider_url, status, return_url, idempotency_key, metadata,
    last_checked_at, expires_at, created_at, updated_at
) VALUES (
    $1,
    sqlc.arg(merchant_id)::uuid,
    $2, sqlc.narg(checkout_session_id), $3, $4, $5, $6, $7,
    sqlc.narg(provider_session_id), $8, $9, sqlc.narg(return_url),
    sqlc.narg(idempotency_key), sqlc.arg(metadata),
    sqlc.narg(last_checked_at), sqlc.narg(expires_at),
    COALESCE(NULLIF(sqlc.arg(created_at)::timestamptz, '0001-01-01 00:00:00+00'::timestamptz), now()),
    COALESCE(NULLIF(sqlc.arg(updated_at)::timestamptz, '0001-01-01 00:00:00+00'::timestamptz), now())
);

-- name: GetUSDCFundingSessionByIDForMerchantSubject :one
SELECT * FROM openrails.usdc_funding_sessions ufs
WHERE ufs.id = $1 AND ufs.merchant_subject_id = $2
LIMIT 1;

-- name: GetUSDCFundingSessionByID :one
SELECT * FROM openrails.usdc_funding_sessions ufs
WHERE ufs.id = $1
LIMIT 1;

-- name: GetUSDCFundingSessionByIdempotencyKey :one
SELECT * FROM openrails.usdc_funding_sessions ufs
WHERE ufs.merchant_subject_id = $1 AND ufs.idempotency_key = $2
LIMIT 1;

-- name: UpdateUSDCFundingSessionStatus :exec
UPDATE openrails.usdc_funding_sessions SET
    status = $2,
    last_checked_at = sqlc.narg(last_checked_at),
    metadata = sqlc.arg(metadata),
    updated_at = sqlc.arg(updated_at)
WHERE id = $1;
