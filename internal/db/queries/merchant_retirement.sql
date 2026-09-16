-- name: ListMerchantRetirementCandidates :many
SELECT id,slug,created_at,permission_group_id::text AS group_id FROM openrails.merchants
WHERE deleted_at IS NULL AND status='active' AND permission_group_id IS NOT NULL
AND created_at < sqlc.arg(created_before)::timestamptz
AND NOT (slug = ANY(sqlc.arg(reserved_slugs)::text[]))
AND (created_at,id) > (sqlc.arg(after_created_at)::timestamptz,sqlc.arg(after_id)::uuid)
ORDER BY created_at,id LIMIT sqlc.arg(page_limit)::bigint;

-- name: LockMerchantRetirementState :one
SELECT slug,permission_group_id,coalesce(deleted_at IS NULL AND status='active',false)::boolean AS live
FROM openrails.merchants WHERE id=sqlc.arg(id)::uuid FOR UPDATE;

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

-- name: MerchantHasActivity :one
-- Retirement blockers: obligations, money history
-- (including tombstones), provider connections, integrations and catalog.
-- Every table here references openrails.merchants, so the retirement row lock
-- serializes concurrent inserts. Tables reached through a NOT NULL foreign key
-- from one of these are implied; the rest are classified in
-- internal/merchants/retirement_activity_integration_test.go.
SELECT coalesce((EXISTS (SELECT 1 FROM openrails.customers WHERE merchant_id = sqlc.arg(merchant_id)::uuid)
	OR EXISTS (SELECT 1 FROM openrails.payments WHERE merchant_id = sqlc.arg(merchant_id)::uuid)
	OR EXISTS (SELECT 1 FROM openrails.subscriptions WHERE merchant_id = sqlc.arg(merchant_id)::uuid)
	OR EXISTS (SELECT 1 FROM openrails.ledger_accounts WHERE merchant_id = sqlc.arg(merchant_id)::uuid)
	OR EXISTS (SELECT 1 FROM openrails.psps WHERE merchant_id = sqlc.arg(merchant_id)::uuid)
	OR EXISTS (SELECT 1 FROM openrails.custodians WHERE merchant_id = sqlc.arg(merchant_id)::uuid)
	OR EXISTS (SELECT 1 FROM openrails.merchant_secrets WHERE merchant_id = sqlc.arg(merchant_id)::uuid)
	OR EXISTS (SELECT 1 FROM openrails.webhook_events WHERE merchant_id = sqlc.arg(merchant_id)::uuid)
	OR EXISTS (SELECT 1 FROM openrails.rail_intents WHERE merchant_id = sqlc.arg(merchant_id)::uuid)
	OR EXISTS (SELECT 1 FROM openrails.host_outbox WHERE merchant_id = sqlc.arg(merchant_id)::uuid)
	OR EXISTS (SELECT 1 FROM openrails.merchant_webhooks WHERE merchant_id = sqlc.arg(merchant_id)::uuid)
	OR EXISTS (SELECT 1 FROM openrails.products WHERE merchant_id = sqlc.arg(merchant_id)::uuid)
	OR EXISTS (SELECT 1 FROM openrails.catalog_meters WHERE merchant_id = sqlc.arg(merchant_id)::uuid)
	OR EXISTS (SELECT 1 FROM openrails.catalog_rate_cards WHERE merchant_id = sqlc.arg(merchant_id)::uuid)
	OR EXISTS (SELECT 1 FROM openrails.billing_policies WHERE merchant_id = sqlc.arg(merchant_id)::uuid)
	OR EXISTS (SELECT 1 FROM openrails.custom_credit_types WHERE merchant_id = sqlc.arg(merchant_id)::uuid)
	OR EXISTS (SELECT 1 FROM openrails.catalog_usage_limits WHERE merchant_id = sqlc.arg(merchant_id)::uuid)
	OR EXISTS (SELECT 1 FROM openrails.catalog_credit_balances WHERE merchant_id = sqlc.arg(merchant_id)::uuid)), false)::boolean AS used;
