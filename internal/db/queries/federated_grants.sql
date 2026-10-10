-- billing.federated_grants: merchant roles granted by invitation to users of
-- trusted issuers. An explicit merchant_id scopes every statement.

-- name: CreateFederatedGrant :one
INSERT INTO billing.federated_grants (merchant_id, email, role)
VALUES (sqlc.arg(merchant_id)::uuid, sqlc.arg(email)::text, sqlc.arg(role)::text)
RETURNING merchant_id, id, email, role, issuer, subject, accepted_at, created_at, updated_at;

-- name: ListFederatedGrants :many
SELECT merchant_id, id, email, role, issuer, subject, accepted_at, created_at, updated_at
FROM billing.federated_grants
WHERE federated_grants.merchant_id = sqlc.arg(merchant_id)::uuid
ORDER BY created_at, id;

-- name: GetFederatedGrant :one
SELECT merchant_id, id, email, role, issuer, subject, accepted_at, created_at, updated_at
FROM billing.federated_grants
WHERE federated_grants.merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid;

-- name: DeleteFederatedGrant :execrows
DELETE FROM billing.federated_grants
WHERE federated_grants.merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid;

-- name: ListFederatedGrantsForSubject :many
-- The accepted grants of one issuer's user on the merchants that issuer is
-- trusted for.
SELECT merchant_id, role
FROM billing.federated_grants
WHERE issuer = sqlc.arg(issuer)::text AND subject = sqlc.arg(subject)::text
  AND federated_grants.merchant_id = ANY(sqlc.arg(merchant_ids)::uuid[]);

-- name: ListPendingFederatedGrantsForEmail :many
SELECT merchant_id, id, role
FROM billing.federated_grants
WHERE email = sqlc.arg(email)::text AND subject IS NULL
  AND federated_grants.merchant_id = ANY(sqlc.arg(merchant_ids)::uuid[])
ORDER BY created_at, id;

-- name: AcceptFederatedGrant :one
UPDATE billing.federated_grants
SET issuer = sqlc.arg(issuer)::text, subject = sqlc.arg(subject)::text, accepted_at = now(), updated_at = now()
WHERE federated_grants.merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid
  AND email = sqlc.arg(email)::text AND subject IS NULL
RETURNING merchant_id, id, email, role, issuer, subject, accepted_at, created_at, updated_at;
