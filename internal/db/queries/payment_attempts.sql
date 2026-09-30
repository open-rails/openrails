-- name: InsertPaymentAttempt :execrows
-- #1110: idempotent on the gateway transaction id, else on the operation step.
INSERT INTO openrails.payment_attempts (
    id, merchant_id, customer_id, psp_id, rail, kind, owner, card_entry, source, observed_via,
    category, reason, action, response_code, response_text, transaction_id, avs_result, cvv_result,
    card_brand, card_last4, token_type, amount, currency, attempted_at, checkout_id, checkout_target,
    subscription_id, payment_method_id, payment_id, rail_intent_id, step, cycle_id,
    card_bin, issuer_code, issuer_text, enriched_at
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
    sqlc.arg(step)::text, sqlc.narg(cycle_id)::uuid,
    sqlc.narg(card_bin)::text, sqlc.narg(issuer_code)::text, sqlc.narg(issuer_text)::text,
    sqlc.narg(enriched_at)::timestamptz
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

-- name: ListUnenrichedAttemptMerchants :many
SELECT merchant_id FROM openrails.unenriched_attempt_merchant_ids(
    sqlc.arg(since)::timestamptz, sqlc.arg(before)::timestamptz, sqlc.arg(merchant_limit)::int);

-- name: ListUnenrichedNMIAttempts :many
-- #1114: NMI attempts in [since, before) the enrichment pass has not read.
SELECT id, psp_id, transaction_id::text AS transaction_id, attempted_at
FROM openrails.payment_attempts
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND enriched_at IS NULL AND rail = 'nmi' AND transaction_id IS NOT NULL
  AND attempted_at >= sqlc.arg(since)::timestamptz AND attempted_at < sqlc.arg(before)::timestamptz
ORDER BY attempted_at, id
LIMIT sqlc.arg(row_limit)::int;

-- name: EnrichPaymentAttempt :execrows
-- #1114: fills what the attempt's own reply lacked from the PSP's
-- transaction read, once. A network token used replaces the token type.
UPDATE openrails.payment_attempts SET
    card_bin = COALESCE(card_bin, sqlc.narg(card_bin)::text),
    card_brand = COALESCE(card_brand, sqlc.narg(card_brand)::text),
    card_last4 = COALESCE(card_last4, sqlc.narg(card_last4)::text),
    avs_result = COALESCE(avs_result, sqlc.narg(avs_result)::text),
    cvv_result = COALESCE(cvv_result, sqlc.narg(cvv_result)::text),
    issuer_code = COALESCE(issuer_code, sqlc.narg(issuer_code)::text),
    issuer_text = COALESCE(issuer_text, sqlc.narg(issuer_text)::text),
    token_type = CASE WHEN sqlc.arg(network_token)::boolean THEN 'network_token' ELSE token_type END,
    enriched_at = sqlc.arg(enriched_at)::timestamptz
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid AND enriched_at IS NULL;

-- name: ListPaymentAttempts :many
-- #1116: the merchant's attempts, newest first; every filter is optional.
SELECT sqlc.embed(a), count(*) OVER () AS total
FROM openrails.payment_attempts a
WHERE a.merchant_id = sqlc.arg(merchant_id)::uuid
  AND (sqlc.narg(kind)::text IS NULL OR a.kind = sqlc.narg(kind)::text)
  AND (sqlc.narg(owner)::text IS NULL OR a.owner = sqlc.narg(owner)::text)
  AND (sqlc.narg(category)::text IS NULL OR a.category = sqlc.narg(category)::text)
  AND (sqlc.narg(reason)::text IS NULL OR a.reason = sqlc.narg(reason)::text)
  AND (sqlc.narg(response_code)::text IS NULL OR a.response_code = sqlc.narg(response_code)::text)
  AND (sqlc.narg(card_entry)::text IS NULL OR a.card_entry = sqlc.narg(card_entry)::text)
  AND (sqlc.narg(psp_id)::uuid IS NULL OR a.psp_id = sqlc.narg(psp_id)::uuid)
  AND (sqlc.narg(customer_id)::uuid IS NULL OR a.customer_id = sqlc.narg(customer_id)::uuid)
  AND (sqlc.narg(checkout_id)::uuid IS NULL OR a.checkout_id = sqlc.narg(checkout_id)::uuid)
  AND (sqlc.narg(subscription_id)::uuid IS NULL OR a.subscription_id = sqlc.narg(subscription_id)::uuid)
  AND (sqlc.narg(cycle_id)::uuid IS NULL OR a.cycle_id = sqlc.narg(cycle_id)::uuid)
  AND (sqlc.narg(since)::timestamptz IS NULL OR a.attempted_at >= sqlc.narg(since)::timestamptz)
  AND (sqlc.narg(until)::timestamptz IS NULL OR a.attempted_at < sqlc.narg(until)::timestamptz)
ORDER BY a.attempted_at DESC, a.id DESC
LIMIT sqlc.arg(page_limit)::int OFFSET sqlc.arg(page_offset)::int;

-- name: GetPaymentAttempt :one
SELECT * FROM openrails.payment_attempts
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid;

-- name: ListCycleAttempts :many
-- #1116: a rebill cycle's attempts, oldest first.
SELECT * FROM openrails.payment_attempts
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND cycle_id = sqlc.arg(cycle_id)::uuid
ORDER BY attempted_at, id;
