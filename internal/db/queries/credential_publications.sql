-- Credential publication receipts contain metadata and references, never secrets.

-- name: CreateCredentialPublication :exec
INSERT INTO openrails.credential_publications (
    merchant_id, operation_id, rail, environment, account_id, expected_revision, request_metadata
) VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (merchant_id, operation_id) DO NOTHING;

-- name: GetCredentialPublication :one
SELECT rail, environment, account_id, expected_revision, request_metadata, result
FROM openrails.credential_publications
WHERE merchant_id = $1 AND operation_id = $2;

-- name: LockCredentialPublication :one
SELECT rail, environment, account_id, expected_revision, request_metadata, result
FROM openrails.credential_publications
WHERE merchant_id = $1 AND operation_id = $2
FOR UPDATE;

-- name: LockCredentialPublicationResult :one
SELECT result FROM openrails.credential_publications
WHERE merchant_id = $1 AND operation_id = $2
FOR UPDATE;

-- name: CompleteCredentialPublication :exec
UPDATE openrails.credential_publications
SET state = 'published', result = $3, published_at = now()
WHERE merchant_id = $1 AND operation_id = $2;

-- name: LockPSPForCredentialPublication :one
SELECT * FROM openrails.psps
WHERE merchant_id = $1 AND rail = $2 AND environment = $3 AND account_id = $4
FOR UPDATE;
