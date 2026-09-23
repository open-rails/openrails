-- #1058 product archive operations and their purchase reviews.

-- name: LockProductArchiveKey :exec
SELECT pg_advisory_xact_lock(hashtextextended(sqlc.arg(lock_key)::text, 0));

-- name: GetProductArchiveByKey :one
SELECT o.id, o.product_id, p.key AS product_key, o.purchase_action, o.purchased_since, o.reason, o.created_at, o.request_sha256
FROM openrails.product_archive_operations o
JOIN openrails.products p ON p.merchant_id = o.merchant_id AND p.id = o.product_id
WHERE o.merchant_id = sqlc.arg(merchant_id)::uuid AND o.idempotency_key = sqlc.arg(idempotency_key)::text;

-- name: GetProductArchiveByID :one
SELECT o.id, o.product_id, p.key AS product_key, o.purchase_action, o.purchased_since, o.reason, o.created_at, o.request_sha256
FROM openrails.product_archive_operations o
JOIN openrails.products p ON p.merchant_id = o.merchant_id AND p.id = o.product_id
WHERE o.merchant_id = sqlc.arg(merchant_id)::uuid AND o.id = sqlc.arg(id)::uuid;

-- name: InsertProductArchive :exec
INSERT INTO openrails.product_archive_operations (merchant_id, idempotency_key, request_sha256, product_id, purchase_action, purchased_since, reason)
VALUES (sqlc.arg(merchant_id)::uuid, sqlc.arg(idempotency_key)::text, sqlc.arg(request_sha256)::bytea, sqlc.arg(product_id)::uuid,
        sqlc.arg(purchase_action)::text, sqlc.narg(purchased_since)::timestamptz, sqlc.arg(reason)::text);

-- One-time completed charges of the product since the window start.
-- Subscription payments stay with their grandfathered subscriptions; rows
-- without money movement qualify only for off-rail channels.
-- name: ListProductArchivePurchases :many
SELECT pay.id, pay.customer_id, pay.amount, pay.currency, pay.purchased_at, pay.money_movement
FROM openrails.payments pay
JOIN openrails.prices pr ON pr.merchant_id = pay.merchant_id AND pr.id = pay.price_id
WHERE pay.merchant_id = sqlc.arg(merchant_id)::uuid AND pr.product_id = sqlc.arg(product_id)::uuid
  AND pay.purchased_at >= sqlc.arg(purchased_since)::timestamptz
  AND pay.refunded_payment_id IS NULL AND pay.amount > 0 AND pay.status = 'completed'
  AND pay.deleted_at IS NULL AND pay.subscription_id IS NULL
  AND (pay.money_movement = 'rail' OR pay.rail IN ('manual', 'admin'))
ORDER BY pay.purchased_at, pay.id;

-- Insert-if-absent: a resolved review is never reopened.
-- name: InsertPurchaseReview :exec
INSERT INTO openrails.reconciliation_findings (merchant_id, finding_type, subject_key, severity, status, recommended_action, evidence)
VALUES (sqlc.arg(merchant_id)::uuid, sqlc.arg(finding_type)::text, sqlc.arg(subject_key)::text, 'medium', 'requires_review',
        sqlc.arg(recommended_action)::text, sqlc.arg(evidence)::jsonb)
ON CONFLICT (merchant_id, finding_type, subject_key) DO NOTHING;

-- name: GetPurchaseReviewBySubject :one
SELECT id, status, evidence, operator_notes, created_at, resolved_at
FROM openrails.reconciliation_findings
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND finding_type = sqlc.arg(finding_type)::text AND subject_key = sqlc.arg(subject_key)::text;

-- name: GetPurchaseReviewByID :one
SELECT id, status, evidence, operator_notes, created_at, resolved_at
FROM openrails.reconciliation_findings
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND finding_type = sqlc.arg(finding_type)::text AND id = sqlc.arg(id)::uuid;

-- name: ListPurchaseReviews :many
SELECT id, status, evidence, operator_notes, created_at, resolved_at
FROM openrails.reconciliation_findings
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND finding_type = sqlc.arg(finding_type)::text
  AND status = ANY(sqlc.arg(statuses)::text[])
  AND (sqlc.arg(product_archive_id)::text = '' OR evidence -> 'local' ->> 'product_archive_id' = sqlc.arg(product_archive_id)::text)
ORDER BY created_at, id
LIMIT sqlc.arg(page_limit) OFFSET sqlc.arg(page_offset);

-- name: CountPurchaseReviews :one
SELECT count(*) FROM openrails.reconciliation_findings
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND finding_type = sqlc.arg(finding_type)::text
  AND status = ANY(sqlc.arg(statuses)::text[])
  AND (sqlc.arg(product_archive_id)::text = '' OR evidence -> 'local' ->> 'product_archive_id' = sqlc.arg(product_archive_id)::text);
