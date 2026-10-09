-- Durable operation-level financial reservations. The immutable body
-- and principals make the operation id a safe replay coordinate; only the
-- three-state terminal transition and extension growth update a row.

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
      FROM billing.cost_qualifications qualification
      WHERE qualification.merchant_id = billing.operation_authorizations.merchant_id
        AND qualification.operation_id = billing.operation_authorizations.operation_id
  )
  AND NOT EXISTS (
      SELECT 1
      FROM billing.cost_refusals refusal
      WHERE refusal.merchant_id = billing.operation_authorizations.merchant_id
        AND refusal.operation_id = billing.operation_authorizations.operation_id
  )
RETURNING *;

-- name: GetOperationAuthorizationExtension :one
SELECT *
FROM billing.operation_authorization_extensions
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND operation_id = sqlc.arg(operation_id)::text
  AND ordinal = sqlc.arg(ordinal)::bigint;

-- name: GetLastOperationAuthorizationExtensionOrdinal :one
SELECT COALESCE(MAX(ordinal), 0)::bigint AS ordinal
FROM billing.operation_authorization_extensions
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND operation_id = sqlc.arg(operation_id)::text;

-- name: ListOperationAuthorizationExtensions :many
SELECT *
FROM billing.operation_authorization_extensions
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND operation_id = sqlc.arg(operation_id)::text
ORDER BY ordinal;

-- name: InsertOperationAuthorizationExtension :one
INSERT INTO billing.operation_authorization_extensions (
    merchant_id, operation_id, ordinal, requested_amount, minimum_amount, granted_amount, authorized_amount
) VALUES (
    sqlc.arg(merchant_id)::uuid,
    sqlc.arg(operation_id)::text,
    sqlc.arg(ordinal)::bigint,
    sqlc.arg(requested_amount)::bigint,
    sqlc.arg(minimum_amount)::bigint,
    sqlc.arg(granted_amount)::bigint,
    sqlc.arg(authorized_amount)::bigint
)
RETURNING *;

-- name: ExtendOperationAuthorization :one
-- Grows an open hold from the extended total the caller read under the payer
-- lock; a stale total or a terminal state matches no row.
UPDATE billing.operation_authorizations
SET extended_amount = extended_amount + sqlc.arg(granted_amount)::bigint
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND operation_id = sqlc.arg(operation_id)::text
  AND state = 'open'
  AND extended_amount = sqlc.arg(extended_amount)::bigint
RETURNING *;

-- A refused hold is released only through a recorded write-off.
-- name: WriteOffOperationAuthorization :one
UPDATE billing.operation_authorizations
SET state = 'released',
    terminal_reference = sqlc.arg(terminal_reference)::text,
    released_at = sqlc.arg(released_at)::timestamptz
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND operation_id = sqlc.arg(operation_id)::text
  AND state = 'open'
  AND EXISTS (
      SELECT 1
      FROM billing.cost_resolutions resolution
      WHERE resolution.merchant_id = billing.operation_authorizations.merchant_id
        AND resolution.operation_id = billing.operation_authorizations.operation_id
        AND resolution.kind = 'written_off'
  )
RETURNING *;

-- Newest first; an empty state filter admits every state, a null refused
-- admits both.
-- name: ListOperationAuthorizations :many
SELECT a.*
FROM billing.operation_authorizations a
WHERE a.merchant_id = sqlc.arg(merchant_id)::uuid
  AND (cardinality(sqlc.arg(states)::text[]) = 0 OR a.state = ANY(sqlc.arg(states)::text[]))
  AND (sqlc.narg(refused)::boolean IS NULL
       OR sqlc.narg(refused)::boolean = EXISTS (
           SELECT 1 FROM billing.cost_refusals r
           WHERE r.merchant_id = a.merchant_id AND r.operation_id = a.operation_id))
  AND (sqlc.narg(after_at)::timestamptz IS NULL
       OR (a.created_at, a.operation_id) < (sqlc.narg(after_at)::timestamptz, sqlc.narg(after_operation_id)::text))
ORDER BY a.created_at DESC, a.operation_id DESC
LIMIT sqlc.arg(row_limit)::int;

-- The refused holds, newest first: driven by the refusals, so a page costs
-- what the refusals do, not every hold the merchant ever opened.
-- name: ListRefusedOperationAuthorizations :many
SELECT sqlc.embed(a)
FROM billing.cost_refusals r
JOIN billing.operation_authorizations a
  ON a.merchant_id = r.merchant_id
 AND a.operation_id = r.operation_id
WHERE r.merchant_id = sqlc.arg(merchant_id)::uuid
  AND (cardinality(sqlc.arg(states)::text[]) = 0 OR a.state = ANY(sqlc.arg(states)::text[]))
  AND (sqlc.narg(after_at)::timestamptz IS NULL
       OR (a.created_at, a.operation_id) < (sqlc.narg(after_at)::timestamptz, sqlc.narg(after_operation_id)::text))
ORDER BY a.created_at DESC, a.operation_id DESC
LIMIT sqlc.arg(row_limit)::int;
