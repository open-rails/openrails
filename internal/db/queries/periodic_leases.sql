-- billing.periodic_leases: deployment-global, no merchant scope.

-- name: ClaimPeriodicLease :execrows
-- Takes the lease on the kind for the merchant set for the holder when it is free, expired or the
-- holder's own; zero rows means another fleet holds it.
INSERT INTO billing.periodic_leases (kind, merchant_set, holder, expires_at)
VALUES (sqlc.arg(kind)::text, sqlc.arg(merchant_set)::text, sqlc.arg(holder)::text, now() + make_interval(secs => sqlc.arg(ttl_seconds)::float8))
ON CONFLICT (kind, merchant_set) DO UPDATE
SET holder = EXCLUDED.holder, expires_at = EXCLUDED.expires_at, claimed_at = now()
WHERE billing.periodic_leases.holder = EXCLUDED.holder OR billing.periodic_leases.expires_at <= now();
