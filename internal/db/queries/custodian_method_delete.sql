-- Admission joins capture/import/remap and user deletion on one vendor handle.
-- name: LockCustodianMethodHandle :exec
SELECT pg_advisory_xact_lock(hashtextextended(sqlc.arg(lock_key)::text, 0));

-- name: CountCustodianMethodAliases :one
SELECT count(*) AS total,
       count(*) FILTER (WHERE customer_id <> sqlc.arg(customer_id)::uuid) AS foreign_payers
FROM openrails.payment_methods
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND custodian_id = sqlc.arg(custodian_id)::uuid
  AND rail_method_ref = sqlc.arg(method_ref)::text;

-- name: CustodianMethodDeletionState :one
-- The vendor never reuses a physically erased permanent method ID. Its
-- retained decision blocks a stale pre-delete capture read even after commit.
SELECT COALESCE(bool_or(status NOT IN ('succeeded','failed_terminal','superseded','expired')),false)::boolean AS pending,
       COALESCE(bool_or(status='succeeded' AND payload->>'detach_only' IS DISTINCT FROM 'true'),false)::boolean AS erased
FROM openrails.rail_intents
WHERE merchant_id=sqlc.arg(merchant_id)::uuid
  AND custodian_id=sqlc.arg(custodian_id)::uuid
  AND intent_type='hyperswitch_method_delete'
  AND payload->'instrument'->>'rail_method_ref'=sqlc.arg(method_ref)::text;

-- name: FencePaymentMethodDeletion :execrows
UPDATE openrails.payment_methods
SET park_reason = 'delete:' || sqlc.arg(operation_id)::uuid::text,
    parked_at = sqlc.arg(now)::timestamptz, updated_at = sqlc.arg(now)::timestamptz
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid
  AND park_reason NOT LIKE 'delete:%';

-- name: DeleteFencedPaymentMethod :execrows
DELETE FROM openrails.payment_methods
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid
  AND park_reason = 'delete:' || sqlc.arg(operation_id)::uuid::text;

-- name: LockCustodianDeletionAccount :one
SELECT * FROM openrails.custodians
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid
FOR SHARE;

-- name: LockCustodianMethodDelete :one
SELECT * FROM openrails.rail_intents
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid
  AND intent_type = 'hyperswitch_method_delete'
FOR UPDATE;

-- name: CompleteCustodianMethodDelete :execrows
UPDATE openrails.rail_intents
SET status = 'succeeded', result_evidence = sqlc.arg(evidence)::jsonb,
    claimed_until = NULL, executed_at = sqlc.arg(now)::timestamptz,
    updated_at = sqlc.arg(now)::timestamptz, last_failure_reason = NULL
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid
  AND intent_type IN ('hyperswitch_method_delete','nmi_vault_delete')
  AND status IN ('in_flight', 'unknown_needs_verify');

-- name: ListMethodDeletesForArchive :many
SELECT * FROM openrails.rail_intents
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND status = 'succeeded'
  AND idempotency_key IN ('hyperswitch_method_delete:' || sqlc.arg(payment_method_id)::uuid::text,
                          'nmi_vault_delete:' || sqlc.arg(payment_method_id)::uuid::text)
  AND NOT EXISTS (SELECT 1 FROM openrails.payment_methods m
                  WHERE m.merchant_id=sqlc.arg(merchant_id)::uuid AND m.id=sqlc.arg(payment_method_id)::uuid);

-- name: LockNativeMethodDelete :one
SELECT * FROM openrails.rail_intents
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid
  AND intent_type = 'nmi_vault_delete'
FOR UPDATE;

-- name: NativeVaultDeletionState :one
SELECT COALESCE(bool_or(status NOT IN ('succeeded','failed_terminal','superseded','expired')),false)::boolean AS pending,
       COALESCE(bool_or(status='succeeded' AND
         (payload->>'billing_entry_only' IS DISTINCT FROM 'true' OR payload->>'rail_method_ref'=sqlc.arg(method_ref)::text)),false)::boolean AS erased
FROM openrails.rail_intents
WHERE merchant_id=sqlc.arg(merchant_id)::uuid AND psp_id=sqlc.arg(psp_id)::uuid
  AND intent_type='nmi_vault_delete' AND payload->>'rail_customer_ref'=sqlc.arg(customer_ref)::text;

-- A PSP account owns the vault namespace; a differently cased imported rail
-- label cannot make another local billing entry disappear from the decision.
-- name: CountNativeVaultAliases :one
SELECT count(*) FROM openrails.payment_methods
WHERE merchant_id=sqlc.arg(merchant_id)::uuid AND psp_id=sqlc.arg(psp_id)::uuid
  AND custodian='psp' AND rail_customer_ref=sqlc.arg(customer_ref)::text
  AND rail_customer_ref<>'' AND id<>sqlc.arg(exclude_id)::uuid;
