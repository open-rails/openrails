-- name: GetConflictingInitialEnrollmentSubscription :one
-- Customer row lock serializes admissions. Unknown obligations retain their
-- exclusion until provider outcome is resolved; a new local row is no handoff.
SELECT s.* FROM openrails.subscriptions s
JOIN openrails.products accepted ON accepted.merchant_id=s.merchant_id AND accepted.id=sqlc.arg(product_id)::uuid
JOIN openrails.products existing ON existing.merchant_id=s.merchant_id AND existing.id=s.product_id
WHERE s.merchant_id=sqlc.arg(merchant_id)::uuid AND s.customer_id=sqlc.arg(customer_id)::uuid
 AND (s.product_id=accepted.id OR (accepted.tier_group IS NOT NULL AND accepted.tier_group<>'' AND existing.tier_group=accepted.tier_group))
 AND s.status IN ('active','pending','past_due','awaiting_method','unverified') AND s.deleted_at IS NULL
ORDER BY s.id LIMIT 1;

-- name: GetConflictingInitialEnrollmentOperation :one
SELECT i.* FROM openrails.rail_intents i
JOIN openrails.products accepted ON accepted.merchant_id=i.merchant_id AND accepted.id=sqlc.arg(product_id)::uuid
JOIN openrails.products existing ON existing.merchant_id=i.merchant_id AND existing.id::text=i.payload->'terms'->>'product_id'
WHERE i.merchant_id=sqlc.arg(merchant_id)::uuid AND i.intent_type='initial_membership'
 AND i.payload->'terms'->>'customer_id'=sqlc.arg(customer_id)::uuid::text
 AND (existing.id=accepted.id OR (accepted.tier_group IS NOT NULL AND accepted.tier_group<>'' AND existing.tier_group=accepted.tier_group))
 AND i.status IN ('pending','in_flight','unknown_needs_verify','failed_retryable')
ORDER BY i.created_at LIMIT 1;
