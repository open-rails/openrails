-- billing.custody_migrations and the payment_methods writes that move an
-- instrument between custodians.

-- name: LockPaymentMethodForCustodyRemap :one
-- The flip is atomic per instrument: take the row lock first so a concurrent
-- charge site reading the same instrument cannot straddle the custody change.
SELECT * FROM billing.payment_methods
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND id = sqlc.arg(id)::uuid
FOR UPDATE;

-- name: CountInFlightChargeIntentsForPaymentMethod :one
-- In-flight or unknown_needs_verify intents of subscriptions charging this
-- method: no charge may straddle the custody flip. Both states clear on their
-- own, so a non-zero count means retry later, not failure.
SELECT count(*)::bigint FROM billing.provider_intents ri
JOIN billing.subscriptions s ON s.merchant_id = ri.merchant_id AND s.id = ri.subscription_id
WHERE s.merchant_id = sqlc.arg(merchant_id)::uuid AND ri.merchant_id = sqlc.arg(merchant_id)::uuid
  AND billing.subscription_payment_method_id(s.merchant_id, s.customer_id, s.payment_method_id, s.price_id, s.rail, s.collection_policy) = sqlc.arg(payment_method_id)::uuid
  AND s.deleted_at IS NULL
  AND ri.status = ANY (ARRAY['in_flight'::text, 'unknown_needs_verify'::text]);

-- name: CountUnresolvedOperationsNamingPaymentMethod :one
-- Unresolved intents whose frozen payload names this method, with or without a
-- subscription (an invoice collection has none). They are judged against the
-- custody they froze, so the flip waits for them.
SELECT count(*)::bigint FROM billing.provider_intents ri
WHERE ri.merchant_id = sqlc.arg(merchant_id)::uuid
  AND ri.status = ANY (ARRAY['pending'::text, 'in_flight'::text, 'failed_retryable'::text, 'unknown_needs_verify'::text])
  AND ((CASE WHEN ri.intent_type='initial_membership' THEN ri.payload->'terms'->>'payment_method_id' ELSE ri.payload->>'payment_method_id' END) = sqlc.arg(payment_method_id)::uuid::text
       OR (ri.intent_type = 'nmi_payment_source_update'
           AND sqlc.arg(payment_method_id)::uuid::text IN (ri.payload->>'new_payment_method_id', ri.payload->>'old_payment_method_id')));

-- name: RemapPaymentMethodCustody :execrows
-- The custody flip: moves the custodian, its handle (rail_method_ref),
-- fingerprint, charge transport and network token, and clears psp_id (routing
-- picks the PSP per charge). id, rail, rail_customer_ref (the forensic link to
-- pre-flip charges) and the card's mandates stay. The WHERE on the current
-- custody is the compare-and-swap against a concurrent second flip.
UPDATE billing.payment_methods SET
    custodian = sqlc.arg(to_custodian)::text,
    custodian_id = sqlc.arg(to_custodian_id)::uuid,
    rail_method_ref = sqlc.arg(to_rail_method_ref)::text,
    fingerprint = COALESCE(NULLIF(sqlc.arg(fingerprint)::text, ''), fingerprint),
    charge_via = COALESCE(NULLIF(sqlc.arg(charge_via)::text, ''), 'pan_proxy'),
    network_token_id = NULLIF(sqlc.arg(network_token_id)::text, ''),
    network_token_status = NULLIF(sqlc.arg(network_token_status)::text, ''),
    network_token_par = NULLIF(sqlc.arg(network_token_par)::text, ''),
    psp_id = NULL,
    card_brand = COALESCE(sqlc.narg(card_brand)::text, card_brand),
    card_last4 = COALESCE(sqlc.narg(card_last4)::text, card_last4),
    card_exp_month = COALESCE(sqlc.narg(card_exp_month)::smallint, card_exp_month),
    card_exp_year = COALESCE(sqlc.narg(card_exp_year)::smallint, card_exp_year),
    updated_at = now()
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND id = sqlc.arg(id)::uuid
  AND custodian = sqlc.arg(from_custodian)::text;

-- name: RecordCustodyMigration :one
INSERT INTO billing.custody_migrations (
    merchant_id, batch_id, payment_method_id, rail,
    from_custodian, from_custodian_id, from_rail_customer_ref, from_rail_method_ref, from_psp_id,
    to_custodian, to_custodian_id, to_rail_method_ref, to_psp_id,
    exported_at, outcome, reason
) VALUES (
    sqlc.arg(merchant_id)::uuid, sqlc.arg(batch_id)::uuid, sqlc.arg(payment_method_id)::uuid, sqlc.arg(rail)::text,
    sqlc.arg(from_custodian)::text, sqlc.narg(from_custodian_id)::uuid,
    NULLIF(sqlc.arg(from_rail_customer_ref)::text, ''), NULLIF(sqlc.arg(from_rail_method_ref)::text, ''), sqlc.narg(from_psp_id)::uuid,
    sqlc.arg(to_custodian)::text, sqlc.arg(to_custodian_id)::uuid, sqlc.arg(to_rail_method_ref)::text,
    sqlc.narg(to_psp_id)::uuid,
    sqlc.narg(exported_at)::timestamptz, sqlc.arg(outcome)::text, NULLIF(sqlc.arg(reason)::text, '')
)
RETURNING *;

-- name: GetPaymentMethodForCustodianToken :one
-- A custodian token addresses exactly one instrument: a second mapping onto it
-- is refused rather than pointing two instruments at one card.
SELECT * FROM billing.payment_methods
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND custodian_id = sqlc.arg(custodian_id)::uuid
  AND custodian = sqlc.arg(custodian)::text
  AND rail_method_ref = sqlc.arg(rail_method_ref)::text
LIMIT 1;
