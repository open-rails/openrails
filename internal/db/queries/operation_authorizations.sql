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
SELECT COALESCE(SUM(GREATEST(authorized_usd_micros - captured_usd_micros, 0)), 0)::bigint AS authorized_usd_micros
FROM openrails.operation_authorizations
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND ledger_account_id = sqlc.arg(ledger_account_id)::uuid
  AND state = 'open';

-- name: GetOperationAuthorizationSettlement :one
SELECT *
FROM openrails.operation_authorization_settlements
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND operation_id = sqlc.arg(operation_id)::text
  AND settlement_id = sqlc.arg(settlement_id)::text;

-- name: InsertOperationAuthorizationSettlement :one
INSERT INTO openrails.operation_authorization_settlements (
    merchant_id,
    operation_id,
    settlement_id,
    amount_usd_micros,
    settlement_body_bytes,
    settlement_body_digest,
    final,
    final_reference
) VALUES (
    sqlc.arg(merchant_id)::uuid,
    sqlc.arg(operation_id)::text,
    sqlc.arg(settlement_id)::text,
    sqlc.arg(amount_usd_micros)::bigint,
    sqlc.arg(settlement_body_bytes)::bytea,
    sqlc.arg(settlement_body_digest)::bytea,
    sqlc.arg(final)::boolean,
    sqlc.narg(final_reference)::text
)
ON CONFLICT (merchant_id, operation_id, settlement_id) DO NOTHING
RETURNING *;

-- name: ApplyOperationAuthorizationSettlement :one
UPDATE openrails.operation_authorizations
SET captured_usd_micros = sqlc.arg(captured_usd_micros)::bigint,
    state = CASE WHEN sqlc.arg(final)::boolean THEN 'settled' ELSE state END,
    terminal_reference = CASE WHEN sqlc.arg(final)::boolean THEN sqlc.narg(final_reference)::text ELSE terminal_reference END,
    settled_at = CASE WHEN sqlc.arg(final)::boolean THEN sqlc.arg(settled_at)::timestamptz ELSE settled_at END
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND operation_id = sqlc.arg(operation_id)::text
  AND state = 'open'
RETURNING *;

-- name: ReleaseOperationAuthorization :one
UPDATE openrails.operation_authorizations
SET state = 'released',
    terminal_reference = sqlc.arg(terminal_reference)::text,
    released_at = sqlc.arg(released_at)::timestamptz
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND operation_id = sqlc.arg(operation_id)::text
  AND state = 'open'
RETURNING *;
