-- name: BeginStripeMethodSetup :execrows
-- Fence setup creation once. Lost responses recover by customer+session metadata.
UPDATE openrails.checkout_sessions
SET status='requires_action',updated_at=sqlc.arg(now)
WHERE merchant_id=sqlc.arg(merchant_id) AND id=sqlc.arg(id)
  AND rail='stripe' AND mode='payment_method' AND status='created'
  AND deleted_at IS NULL AND expires_at>sqlc.arg(now);

-- name: RetainStripeMethodSetup :execrows
UPDATE openrails.checkout_sessions
SET reference=sqlc.arg(reference),updated_at=sqlc.arg(now)
WHERE merchant_id=sqlc.arg(merchant_id) AND id=sqlc.arg(id)
  AND rail='stripe' AND mode='payment_method' AND status='requires_action'
  AND deleted_at IS NULL AND expires_at>sqlc.arg(now)
  AND (reference IS NULL OR reference=sqlc.arg(reference));

-- name: CompleteStripeMethodSetup :execrows
UPDATE openrails.checkout_sessions
SET status='succeeded',rail_state=rail_state || jsonb_build_object('payment_method_id',sqlc.arg(payment_method_id)::uuid::text),updated_at=sqlc.arg(now)
WHERE merchant_id=sqlc.arg(merchant_id) AND id=sqlc.arg(id)
  AND rail='stripe' AND mode='payment_method' AND status='requires_action'
  AND reference=sqlc.arg(reference) AND deleted_at IS NULL AND expires_at>sqlc.arg(now);
