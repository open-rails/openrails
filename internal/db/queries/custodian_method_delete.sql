-- Admission joins capture/import/remap and user deletion on one vendor handle.
-- name: LockCustodianMethodHandle :exec
SELECT pg_advisory_xact_lock(hashtextextended(sqlc.arg(lock_key)::text, 0));

-- name: CountCustodianMethodAliases :one
SELECT count(*) AS total,
       count(*) FILTER (WHERE customer_id <> sqlc.arg(customer_id)::uuid) AS foreign_payers
FROM billing.payment_methods
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND custodian_id = sqlc.arg(custodian_id)::uuid
  AND rail_method_ref = sqlc.arg(method_ref)::text
  AND NOT COALESCE(park_reason LIKE 'removed:%', false);

-- name: CustodianMethodDeletionState :one
-- Retain the decision so a delayed save cannot restore an erased handle.
SELECT COALESCE(bool_or(status NOT IN ('succeeded','failed_terminal','superseded','expired')),false)::boolean AS pending,
       COALESCE(bool_or(status='succeeded' AND payload->>'detach_only' IS DISTINCT FROM 'true'),false)::boolean AS erased
FROM billing.provider_intents
WHERE merchant_id=sqlc.arg(merchant_id)::uuid
  AND custodian_id=sqlc.arg(custodian_id)::uuid
  AND intent_type IN ('hyperswitch_method_delete','platform_method_delete')
  AND payload->'instrument'->>'rail_method_ref'=sqlc.arg(method_ref)::text;

-- name: FencePaymentMethodDeletion :execrows
UPDATE billing.payment_methods
SET park_reason = 'delete:' || sqlc.arg(operation_id)::uuid::text,
    parked_at = sqlc.arg(now)::timestamptz, updated_at = sqlc.arg(now)::timestamptz
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid
  AND (park_reason IS NULL OR park_reason NOT LIKE 'delete:%');

-- name: DeleteFencedPaymentMethod :execrows
DELETE FROM billing.payment_methods
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid
  AND park_reason = 'delete:' || sqlc.arg(operation_id)::uuid::text;

-- name: LockCustodianDeletionAccount :one
SELECT * FROM billing.custodians
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid
FOR SHARE;

-- name: LockCustodianMethodDelete :one
SELECT * FROM billing.provider_intents
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid
  AND intent_type IN ('hyperswitch_method_delete','platform_method_delete')
FOR UPDATE;

-- name: CompleteCustodianMethodDelete :execrows
UPDATE billing.provider_intents
SET status = 'succeeded', result_evidence = sqlc.arg(evidence)::jsonb,
    lease_expires_at = NULL, executed_at = sqlc.arg(now)::timestamptz,
    updated_at = sqlc.arg(now)::timestamptz, last_failure_reason = NULL
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid
  AND intent_type IN ('hyperswitch_method_delete','nmi_vault_delete','platform_method_delete')
  AND status IN ('in_flight', 'unknown_needs_verify');

-- name: ListMethodDeletesForArchive :many
SELECT * FROM billing.provider_intents
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND status = 'succeeded'
  AND idempotency_key IN ('hyperswitch_method_delete:' || sqlc.arg(payment_method_id)::uuid::text,
                          'nmi_vault_delete:' || sqlc.arg(payment_method_id)::uuid::text)
  AND NOT EXISTS (SELECT 1 FROM billing.payment_methods m
                  WHERE m.merchant_id=sqlc.arg(merchant_id)::uuid AND m.id=sqlc.arg(payment_method_id)::uuid);

-- name: LockNativeMethodDelete :one
SELECT * FROM billing.provider_intents
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid
  AND intent_type = 'nmi_vault_delete'
FOR UPDATE;

-- name: NativeVaultDeletionState :one
SELECT COALESCE(bool_or(status NOT IN ('succeeded','failed_terminal','superseded','expired')),false)::boolean AS pending,
       COALESCE(bool_or(status='succeeded' AND
         (payload->>'billing_entry_only' IS DISTINCT FROM 'true' OR payload->>'rail_method_ref'=sqlc.arg(method_ref)::text)),false)::boolean AS erased
FROM billing.provider_intents
WHERE merchant_id=sqlc.arg(merchant_id)::uuid AND psp_id=sqlc.arg(psp_id)::uuid
  AND intent_type='nmi_vault_delete' AND payload->>'rail_customer_ref'=sqlc.arg(customer_ref)::text;

-- A PSP account owns the vault namespace; a differently cased imported rail
-- label cannot make another local billing entry disappear from the decision.
-- name: CountNativeVaultAliases :one
SELECT count(*) FROM billing.payment_methods
WHERE merchant_id=sqlc.arg(merchant_id)::uuid AND psp_id=sqlc.arg(psp_id)::uuid
  AND custodian='psp' AND rail_customer_ref=sqlc.arg(customer_ref)::text
  AND NOT COALESCE(park_reason LIKE 'removed:%', false)
  AND id<>sqlc.arg(exclude_id)::uuid;

-- name: RemoveFencedCustodianPaymentMethod :execrows
-- Keep the row and its versions as evidence for orders and payments. Terminal
-- issuer lifecycle states remain final even when the stored token is erased.
UPDATE billing.payment_methods
SET status = CASE WHEN status = 'active' THEN 'removed' ELSE status END,
    park_reason = 'removed:' || sqlc.arg(operation_id)::uuid::text,
    updated_at = sqlc.arg(now)::timestamptz
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid
  AND custodian IN ('platform','basis_theory','hyperswitch') AND status IN ('active','closed','replaced','removed')
  AND park_reason = 'delete:' || sqlc.arg(operation_id)::uuid::text;

-- name: RemoveFencedNativePaymentMethod :execrows
-- The provider credential is gone, but paid orders and immutable card versions
-- retain their original reference. Keep a nonchargeable completion tombstone.
UPDATE billing.payment_methods
SET status = CASE WHEN status = 'active' THEN 'removed' ELSE status END,
    park_reason = 'removed:' || sqlc.arg(operation_id)::uuid::text,
    updated_at = sqlc.arg(now)::timestamptz
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid
  AND custodian = 'psp' AND rail = 'nmi'
  AND status IN ('active','closed','replaced','removed')
  AND park_reason = 'delete:' || sqlc.arg(operation_id)::uuid::text;

