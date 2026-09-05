-- name: LockMerchantRetirementState :one
SELECT coalesce(deleted_at IS NULL AND status='active' AND permission_group_id=sqlc.arg(group_id)::text,false)::boolean AS live
FROM openrails.merchants WHERE id=sqlc.arg(id)::uuid FOR UPDATE;

-- name: LockMerchantRetirementWarning :one
SELECT first_warned_at FROM openrails.merchant_dormancy_notices
WHERE merchant_id=sqlc.arg(merchant_id)::uuid FOR UPDATE;

-- name: MarkMerchantRetired :exec
UPDATE openrails.merchants SET status='deleted',deleted_at=sqlc.arg(retired_at)::timestamptz,
retired_at=sqlc.arg(retired_at)::timestamptz,updated_at=sqlc.arg(retired_at)::timestamptz
WHERE id=sqlc.arg(id)::uuid;

-- name: CompleteMerchantGroupRelease :exec
UPDATE openrails.merchants SET group_release_completed_at=now(),updated_at=now()
WHERE id=sqlc.arg(id)::uuid AND permission_group_id=sqlc.arg(group_id)::text
AND retired_at IS NOT NULL AND deleted_at IS NOT NULL AND group_release_completed_at IS NULL;

-- name: ListPendingMerchantGroupReleases :many
SELECT id,coalesce(permission_group_id,'')::text AS group_id FROM openrails.merchants
WHERE retired_at IS NOT NULL AND deleted_at IS NOT NULL AND group_release_completed_at IS NULL
ORDER BY retired_at,id LIMIT sqlc.arg(batch_limit)::bigint;

-- name: DeleteMerchantRetirementWarning :exec
DELETE FROM openrails.merchant_dormancy_notices WHERE merchant_id=sqlc.arg(merchant_id)::uuid;

-- name: MerchantHasBillingActivity :one
SELECT coalesce((EXISTS (SELECT 1 FROM openrails.psps             WHERE merchant_id = sqlc.arg(merchant_id)::uuid)
	    OR EXISTS (SELECT 1 FROM openrails.payments         WHERE merchant_id = sqlc.arg(merchant_id)::uuid)
	    OR EXISTS (SELECT 1 FROM openrails.subscriptions    WHERE merchant_id = sqlc.arg(merchant_id)::uuid)
	    OR EXISTS (SELECT 1 FROM openrails.customers        WHERE merchant_id = sqlc.arg(merchant_id)::uuid)
	    OR EXISTS (SELECT 1 FROM openrails.products         WHERE merchant_id = sqlc.arg(merchant_id)::uuid)
	    OR EXISTS (SELECT 1 FROM openrails.ledger_transfers WHERE merchant_id = sqlc.arg(merchant_id)::uuid)), false)::boolean AS used;
