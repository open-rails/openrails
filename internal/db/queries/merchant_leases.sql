-- billing.merchant_leases: a merchant's exclusive work while it runs.

-- name: ClaimMerchantLease :execrows
-- Takes the merchant's lease on the kind for the holder when it is free,
-- expired or the holder's own (a renewal); zero rows means another run has it.
INSERT INTO billing.merchant_leases (merchant_id, kind, holder, expires_at)
VALUES (sqlc.arg(merchant_id)::uuid, sqlc.arg(kind)::text, sqlc.arg(holder)::uuid,
    now() + make_interval(secs => sqlc.arg(ttl_seconds)::float8))
ON CONFLICT (merchant_id, kind) DO UPDATE
SET expires_at = EXCLUDED.expires_at,
    claimed_at = CASE WHEN billing.merchant_leases.holder = EXCLUDED.holder THEN billing.merchant_leases.claimed_at ELSE now() END,
    holder = EXCLUDED.holder
WHERE billing.merchant_leases.holder = EXCLUDED.holder OR billing.merchant_leases.expires_at <= now();

-- name: ReleaseMerchantLease :exec
-- Ends the holder's lease; another holder's is untouched.
DELETE FROM billing.merchant_leases
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND kind = sqlc.arg(kind)::text AND holder = sqlc.arg(holder)::uuid;
