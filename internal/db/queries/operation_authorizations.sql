-- Durable operation-level financial reservations. The immutable body
-- and principals make the operation id a safe replay coordinate; only the
-- three-state terminal transition may update a row.

-- name: InsertOperationAuthorization :one
INSERT INTO billing.operation_authorizations (
    operation_id,
    merchant_id,
    customer_id,
    record_owner,
    ledger_account_id,
    currency,
    amount,
    claim_reference,
    authorization_body_bytes,
    authorization_body_digest
) VALUES (
    sqlc.arg(operation_id)::text,
    sqlc.arg(merchant_id)::uuid,
    sqlc.arg(customer_id)::uuid,
    sqlc.arg(record_owner)::text,
    sqlc.arg(ledger_account_id)::uuid,
    sqlc.arg(currency)::text,
    sqlc.arg(amount)::bigint,
    sqlc.arg(claim_reference)::text,
    sqlc.arg(authorization_body_bytes)::bytea,
    sqlc.arg(authorization_body_digest)::bytea
)
ON CONFLICT (merchant_id, operation_id) DO NOTHING
RETURNING *;

-- name: GetOperationAuthorization :one
SELECT *
FROM billing.operation_authorizations
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND operation_id = sqlc.arg(operation_id)::text;

-- name: SettleOperationAuthorizationPassThroughProviderCost :one
UPDATE billing.operation_authorizations
SET state = 'settled',
    settlement_cost_amount = sqlc.arg(settlement_cost_amount)::bigint,
    settlement_amount = sqlc.arg(settlement_amount)::bigint,
    settlement_body_bytes = sqlc.arg(settlement_body_bytes)::bytea,
    settlement_body_digest = sqlc.arg(settlement_body_digest)::bytea,
    terminal_reference = sqlc.arg(terminal_reference)::text,
    settled_at = sqlc.arg(settled_at)::timestamptz
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND operation_id = sqlc.arg(operation_id)::text
  AND state = 'open'
RETURNING *;

-- name: ReleaseOperationAuthorization :one
UPDATE billing.operation_authorizations
SET state = 'released',
    terminal_reference = sqlc.arg(terminal_reference)::text,
    released_at = sqlc.arg(released_at)::timestamptz
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND operation_id = sqlc.arg(operation_id)::text
  AND state = 'open'
  AND NOT EXISTS (
      SELECT 1
      FROM billing.provider_billing_qualifications qualification
      WHERE qualification.merchant_id = billing.operation_authorizations.merchant_id
        AND qualification.operation_id = billing.operation_authorizations.operation_id
  )
RETURNING *;
