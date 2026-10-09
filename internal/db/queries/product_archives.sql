-- Product archive operations and the findings that hold their purchases for review.

-- name: LockProductArchiveKey :exec
SELECT pg_advisory_xact_lock(hashtextextended(sqlc.arg(lock_key)::text, 0));

-- name: GetProductArchiveByKey :one
SELECT o.id, o.product_id, p.key AS product_key, o.purchase_action, o.purchase_window_starts_at, o.reason, o.created_at, o.request_sha256
FROM billing.product_archive_operations o
JOIN billing.products p ON p.merchant_id = o.merchant_id AND p.id = o.product_id
WHERE o.merchant_id = sqlc.arg(merchant_id)::uuid AND o.idempotency_key = sqlc.arg(idempotency_key)::text;

-- name: InsertProductArchive :exec
INSERT INTO billing.product_archive_operations (merchant_id, idempotency_key, request_sha256, product_id, purchase_action, purchase_window_starts_at, reason)
VALUES (sqlc.arg(merchant_id)::uuid, sqlc.arg(idempotency_key)::text, sqlc.arg(request_sha256)::bytea, sqlc.arg(product_id)::uuid,
        sqlc.arg(purchase_action)::text, sqlc.narg(purchase_window_starts_at)::timestamptz, NULLIF(sqlc.arg(reason)::text, ''));

-- One-time completed charges of the product since the window start.
-- Subscription payments stay with their grandfathered subscriptions; rows
-- without money movement qualify only for off-rail channels.
-- name: ListProductArchivePurchases :many
SELECT pay.id, pay.customer_id, pay.amount, pay.currency, pay.purchased_at, pay.money_movement
FROM billing.payments pay
JOIN billing.prices pr ON pr.merchant_id = pay.merchant_id AND pr.id = pay.price_id
WHERE pay.merchant_id = sqlc.arg(merchant_id)::uuid AND pr.product_id = sqlc.arg(product_id)::uuid
  AND pay.purchased_at >= sqlc.arg(purchase_window_starts_at)::timestamptz
  AND pay.refunded_payment_id IS NULL AND pay.amount > 0 AND pay.status = 'succeeded'
  AND pay.deleted_at IS NULL AND pay.subscription_id IS NULL
  AND (pay.money_movement = 'rail' OR pay.channel <> 'rail')
ORDER BY pay.purchased_at, pay.id;

-- Insert-if-absent: a resolved review is never reopened.
-- name: InsertPurchaseReview :exec
INSERT INTO billing.reconciliation_findings (merchant_id, finding_type, subject_key, severity, status, recommended_action, evidence)
VALUES (sqlc.arg(merchant_id)::uuid, sqlc.arg(finding_type)::text, sqlc.arg(subject_key)::text, 'medium', 'requires_review',
        sqlc.arg(recommended_action)::text, sqlc.arg(evidence)::jsonb)
ON CONFLICT (merchant_id, finding_type, psp_id, subject_key) DO NOTHING;

-- name: GetPurchaseReviewBySubject :one
SELECT id, status, evidence, operator_notes, created_at, resolved_at
FROM billing.reconciliation_findings
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND finding_type = sqlc.arg(finding_type)::text AND subject_key = sqlc.arg(subject_key)::text;
