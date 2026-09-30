-- name: InsertPaymentAttempt :execrows
-- #1110: idempotent on the gateway transaction id, else on the operation step.
INSERT INTO openrails.payment_attempts (
    id, merchant_id, customer_id, psp_id, rail, kind, owner, card_entry, source, observed_via,
    category, reason, action, response_code, response_text, transaction_id, avs_result, cvv_result,
    card_brand, card_last4, token_type, amount, currency, attempted_at, checkout_id, checkout_target,
    subscription_id, payment_method_id, payment_id, rail_intent_id, step
) VALUES (
    sqlc.arg(id)::uuid, sqlc.arg(merchant_id)::uuid, sqlc.arg(customer_id)::uuid, sqlc.arg(psp_id)::uuid,
    sqlc.arg(rail)::text, sqlc.arg(kind)::text, sqlc.arg(owner)::text, sqlc.arg(card_entry)::text,
    sqlc.arg(source)::text, sqlc.arg(observed_via)::text, sqlc.arg(category)::text, sqlc.narg(reason)::text,
    sqlc.narg(action)::text, sqlc.narg(response_code)::text, sqlc.narg(response_text)::text,
    sqlc.narg(transaction_id)::text, sqlc.narg(avs_result)::text, sqlc.narg(cvv_result)::text,
    sqlc.narg(card_brand)::text, sqlc.narg(card_last4)::text, sqlc.narg(token_type)::text,
    sqlc.arg(amount)::bigint, sqlc.narg(currency)::text, sqlc.arg(attempted_at)::timestamptz,
    sqlc.narg(checkout_id)::uuid, sqlc.narg(checkout_target)::text, sqlc.narg(subscription_id)::uuid,
    sqlc.narg(payment_method_id)::uuid, sqlc.narg(payment_id)::uuid, sqlc.narg(rail_intent_id)::uuid,
    sqlc.arg(step)::text
)
ON CONFLICT DO NOTHING;

-- name: LatestPaymentCheckoutAttempt :one
-- The buyer's most recent attempt on one checkout target since a moment.
SELECT checkout_id::uuid AS checkout_id, kind, category
FROM openrails.payment_attempts
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND customer_id = sqlc.arg(customer_id)::uuid
  AND checkout_target = sqlc.arg(checkout_target)::text
  AND attempted_at > sqlc.arg(since)::timestamptz
ORDER BY attempted_at DESC, id DESC
LIMIT 1;

-- name: CheckoutVerifiedPaymentMethod :one
-- Whether this checkout itself verified the card, i.e. the buyer typed it here.
SELECT EXISTS (
    SELECT 1 FROM openrails.payment_attempts
    WHERE merchant_id = sqlc.arg(merchant_id)::uuid
      AND customer_id = sqlc.arg(customer_id)::uuid
      AND checkout_target = sqlc.arg(checkout_target)::text
      AND checkout_id = sqlc.arg(checkout_id)::uuid
      AND payment_method_id = sqlc.arg(payment_method_id)::uuid
      AND kind = 'verify' AND category = 'approved'
)::boolean AS verified;
