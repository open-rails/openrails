-- The row lock and the reads that guard a change to a saved card's
-- custody.

-- name: LockPaymentMethodForCustodyRemap :one
-- Take the row lock first so a concurrent charge site reading the same
-- instrument cannot straddle a change to its custody.
SELECT * FROM billing.payment_methods
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND id = sqlc.arg(id)::uuid
FOR UPDATE;

-- name: CountInFlightChargeIntentsForPaymentMethod :one
-- In-flight or unknown_needs_verify intents of subscriptions charging this
-- method: no charge may straddle a custody change. Both states clear on their
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
-- custody they froze, so a custody change waits for them.
SELECT count(*)::bigint FROM billing.provider_intents ri
WHERE ri.merchant_id = sqlc.arg(merchant_id)::uuid
  AND ri.status = ANY (ARRAY['pending'::text, 'in_flight'::text, 'failed_retryable'::text, 'unknown_needs_verify'::text])
  AND ((CASE WHEN ri.intent_type='initial_membership' THEN ri.payload->'terms'->>'payment_method_id' ELSE ri.payload->>'payment_method_id' END) = sqlc.arg(payment_method_id)::uuid::text
       OR (ri.intent_type = 'nmi_payment_source_update'
           AND sqlc.arg(payment_method_id)::uuid::text IN (ri.payload->>'new_payment_method_id', ri.payload->>'old_payment_method_id')));

-- name: GetPaymentMethodForCustodianToken :one
-- The instrument a custodian token addresses.
SELECT * FROM billing.payment_methods
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND custodian_id = sqlc.arg(custodian_id)::uuid
  AND custodian = sqlc.arg(custodian)::text
  AND rail_method_ref = sqlc.arg(rail_method_ref)::text
LIMIT 1;
