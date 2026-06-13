-- #348 probe-cooldown cache. openrails.probe_verdicts is RLS-exempt (global,
-- instance-level credential state) so these statements work on an ordinary
-- non-tenant-pinned boot connection.

-- name: GetProbeVerdict :one
SELECT verdict, checked_at
FROM openrails.probe_verdicts
WHERE provider = sqlc.arg(provider) AND key_hash = sqlc.arg(key_hash);

-- name: UpsertProbeVerdict :exec
INSERT INTO openrails.probe_verdicts (provider, key_hash, verdict, checked_at)
VALUES (sqlc.arg(provider), sqlc.arg(key_hash), sqlc.arg(verdict), now())
ON CONFLICT (provider, key_hash) DO UPDATE SET
    verdict = EXCLUDED.verdict,
    checked_at = now();
