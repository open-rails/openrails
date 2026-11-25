-- name: InsertPaymentAttempt :execrows
-- Idempotent on the gateway transaction id, else on the operation step.
INSERT INTO billing.payment_attempts (
    id, merchant_id, customer_id, psp_id, rail, kind, owner, card_entry, source, observed_via,
    outcome, decline_cause, reason, action, response_code, response_text, transaction_id, avs_result, cvv_result,
    card_brand, card_last4, token_type, amount, currency, attempted_at, purchase_id, purchase_target,
    subscription_id, payment_method_id, payment_id, provider_intent_id, step, cycle_id,
    card_bin, issuer_code, issuer_text, mandate_id, sent_initial_transaction_id, invoice_id,
    advice_code, network_advice_code
) VALUES (
    sqlc.arg(id)::uuid, sqlc.arg(merchant_id)::uuid, sqlc.arg(customer_id)::uuid, sqlc.arg(psp_id)::uuid,
    sqlc.arg(rail)::text, sqlc.arg(kind)::text, sqlc.arg(owner)::text, sqlc.arg(card_entry)::text,
    sqlc.arg(source)::text, sqlc.arg(observed_via)::text, sqlc.arg(outcome)::text, sqlc.narg(decline_cause)::text, sqlc.narg(reason)::text,
    sqlc.narg(action)::text, sqlc.narg(response_code)::text, sqlc.narg(response_text)::text,
    sqlc.narg(transaction_id)::text, sqlc.narg(avs_result)::text, sqlc.narg(cvv_result)::text,
    sqlc.narg(card_brand)::text, sqlc.narg(card_last4)::text, sqlc.narg(token_type)::text,
    sqlc.arg(amount)::bigint, sqlc.narg(currency)::text, sqlc.arg(attempted_at)::timestamptz,
    sqlc.narg(purchase_id)::uuid, sqlc.narg(purchase_target)::text, sqlc.narg(subscription_id)::uuid,
    sqlc.narg(payment_method_id)::uuid, sqlc.narg(payment_id)::uuid, sqlc.narg(provider_intent_id)::uuid,
    sqlc.arg(step)::text, sqlc.narg(cycle_id)::uuid,
    sqlc.narg(card_bin)::text, sqlc.narg(issuer_code)::text, sqlc.narg(issuer_text)::text,
    sqlc.narg(mandate_id)::uuid, sqlc.narg(sent_initial_transaction_id)::text,
    sqlc.narg(invoice_id)::uuid, sqlc.narg(advice_code)::text, sqlc.narg(network_advice_code)::text
)
ON CONFLICT DO NOTHING;

-- name: LatestPaymentInvoiceAttempt :one
-- The buyer's most recent attempt on one purchase target since a moment.
SELECT purchase_id::uuid AS purchase_id, kind, outcome
FROM billing.payment_attempts
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND customer_id = sqlc.arg(customer_id)::uuid
  AND purchase_target = sqlc.arg(purchase_target)::text
  AND attempted_at > sqlc.arg(since)::timestamptz
ORDER BY attempted_at DESC, id DESC
LIMIT 1;

-- name: PurchaseVerifiedPaymentMethod :one
-- Whether this checkout itself verified the card, i.e. the buyer typed it here.
SELECT EXISTS (
    SELECT 1 FROM billing.payment_attempts
    WHERE merchant_id = sqlc.arg(merchant_id)::uuid
      AND customer_id = sqlc.arg(customer_id)::uuid
      AND purchase_target = sqlc.arg(purchase_target)::text
      AND purchase_id = sqlc.arg(purchase_id)::uuid
      AND payment_method_id = sqlc.arg(payment_method_id)::uuid
      AND kind = 'verify' AND outcome = 'approved'
)::boolean AS verified;

-- name: ListPaymentAttempts :many
-- One page of the merchant's attempts, newest first, after an (attempted_at, id)
-- cursor; every filter is optional and a text filter matches any of its values.
SELECT a.*
FROM billing.payment_attempts a
WHERE a.merchant_id = sqlc.arg(merchant_id)::uuid
  AND (sqlc.narg(kinds)::text[] IS NULL OR a.kind = ANY(sqlc.narg(kinds)::text[]))
  AND (sqlc.narg(owners)::text[] IS NULL OR a.owner = ANY(sqlc.narg(owners)::text[]))
  AND (sqlc.narg(outcomes)::text[] IS NULL OR a.outcome = ANY(sqlc.narg(outcomes)::text[]))
  AND (sqlc.narg(decline_causes)::text[] IS NULL OR a.decline_cause = ANY(sqlc.narg(decline_causes)::text[]))
  AND (sqlc.narg(reasons)::text[] IS NULL OR a.reason = ANY(sqlc.narg(reasons)::text[]))
  AND (sqlc.narg(response_codes)::text[] IS NULL OR a.response_code = ANY(sqlc.narg(response_codes)::text[]))
  AND (sqlc.narg(card_entries)::text[] IS NULL OR a.card_entry = ANY(sqlc.narg(card_entries)::text[]))
  AND (sqlc.narg(sources)::text[] IS NULL OR a.source = ANY(sqlc.narg(sources)::text[]))
  AND (sqlc.narg(observed_vias)::text[] IS NULL OR a.observed_via = ANY(sqlc.narg(observed_vias)::text[]))
  AND (sqlc.narg(avs_results)::text[] IS NULL OR a.avs_result = ANY(sqlc.narg(avs_results)::text[]))
  AND (sqlc.narg(cvv_results)::text[] IS NULL OR a.cvv_result = ANY(sqlc.narg(cvv_results)::text[]))
  AND (sqlc.narg(psp_id)::uuid IS NULL OR a.psp_id = sqlc.narg(psp_id)::uuid)
  AND (sqlc.narg(customer_id)::uuid IS NULL OR a.customer_id = sqlc.narg(customer_id)::uuid)
  AND (sqlc.narg(payment_id)::uuid IS NULL OR a.payment_id = sqlc.narg(payment_id)::uuid)
  AND (sqlc.narg(invoice_id)::uuid IS NULL OR a.invoice_id = sqlc.narg(invoice_id)::uuid)
  AND (sqlc.narg(subscription_id)::uuid IS NULL OR a.subscription_id = sqlc.narg(subscription_id)::uuid)
  AND (sqlc.narg(cycle_id)::uuid IS NULL OR a.cycle_id = sqlc.narg(cycle_id)::uuid)
  AND (sqlc.narg(since)::timestamptz IS NULL OR a.attempted_at >= sqlc.narg(since)::timestamptz)
  AND (sqlc.narg(until)::timestamptz IS NULL OR a.attempted_at < sqlc.narg(until)::timestamptz)
  AND (sqlc.narg(after_at)::timestamptz IS NULL
       OR (a.attempted_at, a.id) < (sqlc.narg(after_at)::timestamptz, sqlc.narg(after_id)::uuid))
ORDER BY a.attempted_at DESC, a.id DESC
LIMIT sqlc.arg(row_limit)::int;

-- name: ListPaymentAttemptsByIDs :many
SELECT * FROM billing.payment_attempts
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = ANY(sqlc.arg(ids)::uuid[])
ORDER BY attempted_at DESC, id DESC;

-- name: GetPaymentAttempt :one
SELECT * FROM billing.payment_attempts
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid;

-- name: ListCycleAttempts :many
-- A rebill cycle's attempts, oldest first.
SELECT * FROM billing.payment_attempts
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND cycle_id = sqlc.arg(cycle_id)::uuid
ORDER BY attempted_at, id;

-- Attempts past their retention, batched: row_limit bounds one statement and
-- the cleanup worker loops.
-- name: DeletePaymentAttemptsBefore :execrows
DELETE FROM billing.payment_attempts
WHERE id IN (
    SELECT a.id FROM billing.payment_attempts a
    WHERE a.merchant_id = sqlc.arg(merchant_id)::uuid
      AND a.attempted_at < sqlc.arg(cutoff)::timestamptz
    LIMIT sqlc.arg(row_limit)::int
);

-- name: GetCycleCardStanding :one
-- The declines of the renewal due at due_at on one card: how many, how many
-- the customer made, and the latest one's reason, action and issuer code.
SELECT count(*)::int AS declines,
       count(*) FILTER (WHERE pa.kind = 'customer_retry')::int AS customer_declines,
       coalesce((array_agg(pa.reason ORDER BY pa.attempted_at DESC, pa.id DESC))[1], '')::text AS reason,
       coalesce((array_agg(pa.action ORDER BY pa.attempted_at DESC, pa.id DESC))[1], '')::text AS action,
       coalesce((array_agg(pa.issuer_code ORDER BY pa.attempted_at DESC, pa.id DESC))[1], '')::text AS issuer_code
FROM billing.rebill_cycles c
JOIN billing.payment_attempts pa ON pa.merchant_id = c.merchant_id AND pa.cycle_id = c.id
WHERE c.merchant_id = sqlc.arg(merchant_id)::uuid
  AND c.subscription_id = sqlc.arg(subscription_id)::uuid
  AND c.due_at = sqlc.arg(due_at)::timestamptz
  AND pa.outcome <> 'approved'
  AND pa.payment_method_id = sqlc.arg(payment_method_id)::uuid;
