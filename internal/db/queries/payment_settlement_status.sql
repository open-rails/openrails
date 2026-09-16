-- name: HasSettledPayment :one
-- Durable settlement history, independent of host-event acknowledgment/retention.
SELECT EXISTS (
    SELECT 1 FROM openrails.payments
    WHERE merchant_id = sqlc.arg(merchant_id)::uuid
      AND customer_id = sqlc.arg(customer_id)::uuid
      AND price_id = sqlc.arg(price_id)::uuid
      AND status IN ('completed', 'refunded')
      AND money_movement = 'rail'
      AND amount > 0
      AND refunded_payment_id IS NULL
)::boolean;
