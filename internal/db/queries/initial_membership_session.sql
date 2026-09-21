-- name: CompleteInitialMembershipSession :execrows
-- The accepted operation owns its terminal checkout projection. Expiry is only
-- an offer deadline and cannot erase an accepted financial operation.
UPDATE openrails.checkout_sessions
SET status=sqlc.arg(status)::text,payment_id=sqlc.narg(payment_id)::uuid,
    subscription_id=sqlc.narg(subscription_id)::uuid,transaction_id=sqlc.narg(transaction_id)::text,
    expires_at=NULL,updated_at=sqlc.arg(now)
WHERE merchant_id=sqlc.arg(merchant_id)::uuid AND id=sqlc.arg(id)::uuid
  AND customer_id=sqlc.arg(customer_id)::uuid AND psp_id=sqlc.arg(psp_id)::uuid
  AND price_id=sqlc.arg(price_id)::uuid AND rail=sqlc.arg(rail)::text
  AND mode='subscription' AND deleted_at IS NULL
  AND rail_state ? 'initial_membership_quote'
  AND (status NOT IN ('succeeded','failed') OR status=sqlc.arg(status)::text);
