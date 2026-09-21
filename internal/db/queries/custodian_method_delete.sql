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

-- name: CustodianMethodDeletionPending :one
SELECT EXISTS (
    SELECT 1 FROM openrails.rail_intents
    WHERE merchant_id = sqlc.arg(merchant_id)::uuid
      AND custodian_id = sqlc.arg(custodian_id)::uuid
      AND intent_type = 'hyperswitch_method_delete'
      AND status NOT IN ('succeeded', 'failed_terminal', 'superseded', 'expired')
      AND payload->'instrument'->>'rail_method_ref' = sqlc.arg(method_ref)::text
) AS pending;

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
