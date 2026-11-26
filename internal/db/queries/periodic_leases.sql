-- billing.periodic_leases: deployment-global, no merchant scope.

-- name: ClaimPeriodicLease :execrows
-- Takes the lease on the kind for the holder when it is free, expired or the
-- holder's own (a renewal); zero rows means another holder has it.
INSERT INTO billing.periodic_leases (kind, holder, expires_at)
VALUES (sqlc.arg(kind)::text, sqlc.arg(holder)::text, now() + make_interval(secs => sqlc.arg(ttl_seconds)::float8))
ON CONFLICT (kind) DO UPDATE
SET holder = EXCLUDED.holder, expires_at = EXCLUDED.expires_at, claimed_at = now()
WHERE billing.periodic_leases.holder = EXCLUDED.holder OR billing.periodic_leases.expires_at <= now();

-- name: GetPeriodicLeaseClaim :one
-- The fleet that last claimed the kind, and when: the last to queue its pass.
SELECT holder, claimed_at FROM billing.periodic_leases
WHERE kind = sqlc.arg(kind)::text;

-- name: ReleasePeriodicLease :exec
-- Ends the holder's lease on the kind; another holder's is untouched.
DELETE FROM billing.periodic_leases
WHERE kind = sqlc.arg(kind)::text AND holder = sqlc.arg(holder)::text;
