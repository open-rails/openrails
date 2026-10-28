-- billing.periodic_leases: deployment-global, no merchant scope.

-- name: ClaimPeriodicLease :execrows
-- Takes the lease on the kind for the holder when it is free, expired or the
-- holder's own; zero rows means another fleet holds it.
INSERT INTO billing.periodic_leases (kind, holder, expires_at)
VALUES (sqlc.arg(kind)::text, sqlc.arg(holder)::text, now() + make_interval(secs => sqlc.arg(ttl_seconds)::float8))
ON CONFLICT (kind) DO UPDATE
SET holder = EXCLUDED.holder, expires_at = EXCLUDED.expires_at, claimed_at = now()
WHERE billing.periodic_leases.holder = EXCLUDED.holder OR billing.periodic_leases.expires_at <= now();

-- name: ListPeriodicLeasesHeldElsewhere :many
-- The kinds whose live lease another fleet holds.
SELECT kind FROM billing.periodic_leases
WHERE kind = ANY(sqlc.arg(kinds)::text[]) AND holder <> sqlc.arg(holder)::text AND expires_at > now();
