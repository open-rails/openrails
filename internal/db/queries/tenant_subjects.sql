-- billing.tenant_subjects: payable-subject resolution (#317).

-- name: UpsertSelfTenantSubject :one
-- Self-service identities are their own UUID: materialize the row WITH
-- id = the subject UUID and return it unchanged (refreshing last_seen_at).
INSERT INTO billing.tenant_subjects (id, tenant_id, issuer, subject)
VALUES ($1, $2, $3, $4)
ON CONFLICT (id) DO UPDATE SET last_seen_at = now()
RETURNING id;

-- name: UpsertLegacyTenantSubject :one
-- Non-UUID external subject: keyed by (tenant, issuer, subject) with a
-- generated id.
INSERT INTO billing.tenant_subjects (tenant_id, issuer, subject)
VALUES ($1, $2, $3)
ON CONFLICT (tenant_id, issuer, subject) DO UPDATE SET last_seen_at = now()
RETURNING id;

-- name: GetTenantSubjectIDByIssuerSubject :one
SELECT id FROM billing.tenant_subjects
WHERE tenant_id = $1 AND issuer = $2 AND subject = $3;

-- name: EnsureTenantSubjectRow :exec
-- FK-target materialization before commerce inserts; no-op when present.
INSERT INTO billing.tenant_subjects (id, tenant_id, issuer, subject)
VALUES ($1, $2, $3, $4)
ON CONFLICT DO NOTHING;
