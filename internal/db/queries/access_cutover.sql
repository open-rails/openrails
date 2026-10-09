-- The cutover from per-key entitlement windows to product access. source names
-- the relation holding the per-key windows: billing.entitlements before
-- migration 17, or an older archive's rows during its restore.

-- name: ConvertEntitlementWindows :one
SELECT billing.convert_entitlement_windows(sqlc.arg(merchant_id)::uuid, sqlc.arg(source)::text::regclass, sqlc.arg(at)::timestamptz)::bigint AS converted;

-- name: ListProductAccessChanges :many
SELECT c.customer_id::uuid AS customer_id, c.entitlement::text AS entitlement, c.change::text AS change
FROM billing.product_access_changes(sqlc.arg(merchant_id)::uuid, sqlc.arg(source)::text::regclass, sqlc.arg(at)::timestamptz) c;

-- name: ListProductAccessConversionNotes :many
SELECT n.customer_id::uuid AS customer_id, n.note::text AS note, n.source_type::text AS source_type,
       n.source_id::text AS source_id, n.entitlements::text[] AS entitlements
FROM billing.product_access_conversion_notes(sqlc.arg(merchant_id)::uuid, sqlc.arg(source)::text::regclass, sqlc.arg(at)::timestamptz) n;

-- name: ApproveAccessCutoverChange :exec
INSERT INTO billing.access_cutover_approvals (merchant_id, customer_id, entitlement, change, approved_by)
VALUES (sqlc.arg(merchant_id)::uuid, sqlc.arg(customer_id)::uuid, sqlc.arg(entitlement)::text, sqlc.arg(change)::text, sqlc.arg(approved_by)::text)
ON CONFLICT (merchant_id, customer_id, entitlement, change) DO NOTHING;

-- name: ListAccessCutoverApprovals :many
SELECT customer_id, entitlement::text AS entitlement, change FROM billing.access_cutover_approvals
WHERE merchant_id = sqlc.arg(merchant_id)::uuid;
