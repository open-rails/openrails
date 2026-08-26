-- th-005 durable operation-level financial reservations. The immutable body
-- and principals make the operation id a safe replay coordinate; only the
-- three-state terminal transition may update a row.

-- name: InsertOperationAuthorization :one
INSERT INTO openrails.operation_authorizations (
    operation_id,
    merchant_id,
    payer_id,
    record_owner,
    ledger_account_id,
    authorized_usd_micros,
    claim_reference,
    authorization_body_bytes,
    authorization_body_digest
) VALUES (
    sqlc.arg(operation_id)::text,
    sqlc.arg(merchant_id)::uuid,
    sqlc.arg(payer_id)::uuid,
    sqlc.arg(record_owner)::text,
    sqlc.arg(ledger_account_id)::uuid,
    sqlc.arg(authorized_usd_micros)::bigint,
    sqlc.arg(claim_reference)::text,
    sqlc.arg(authorization_body_bytes)::bytea,
    sqlc.arg(authorization_body_digest)::bytea
)
ON CONFLICT (merchant_id, operation_id) DO NOTHING
RETURNING *;

-- name: GetOperationAuthorization :one
SELECT *
FROM openrails.operation_authorizations
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND operation_id = sqlc.arg(operation_id)::text;

-- name: SumOpenOperationAuthorizationMicros :one
SELECT COALESCE(SUM(authorized_usd_micros), 0)::bigint AS authorized_usd_micros
FROM openrails.operation_authorizations
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND ledger_account_id = sqlc.arg(ledger_account_id)::uuid
  AND state = 'open';

-- name: ReleaseOperationAuthorization :one
UPDATE openrails.operation_authorizations
SET state = 'released',
    terminal_reference = sqlc.arg(terminal_reference)::text,
    released_at = sqlc.arg(released_at)::timestamptz
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND operation_id = sqlc.arg(operation_id)::text
  AND state = 'open'
RETURNING *;
