-- Exact access is a grant-ledger projection, independent of mutable catalog.
-- name: CheckResourceEntitlements :many
SELECT candidate.entitlement::text AS entitlement, EXISTS (
 SELECT 1 FROM openrails.entitlements e
 WHERE e.merchant_id=sqlc.arg(merchant_id)::uuid
   AND e.customer_id=sqlc.arg(customer_id)::uuid AND e.entitlement=candidate.entitlement
   AND e.start_at<=sqlc.arg(at_time)::timestamptz
   AND (e.end_at IS NULL OR e.end_at>sqlc.arg(at_time)::timestamptz)
   AND e.revoked_at IS NULL AND e.deleted_at IS NULL
) AS has_access
FROM unnest(sqlc.arg(entitlements)::text[]) AS candidate(entitlement);

-- Discovery only: all pricing and benefits are revalidated at admission.
-- One page per requested key; uuid.Nil in after_ids starts a key's first page.
-- name: ListOffersForEntitlements :many
SELECT wanted.entitlement::text AS entitlement, offer.product_id, offer.product_key,
 offer.product_name, offer.entitlements_spec, offer.price_id, offer.price_key,
 offer.unit_amount, offer.currency, offer.access_duration_hours, offer.auto_renew
FROM unnest(sqlc.arg(entitlements)::text[], sqlc.arg(after_currencies)::text[], sqlc.arg(after_ids)::uuid[])
 AS wanted(entitlement, after_currency, after_id)
CROSS JOIN LATERAL (
 SELECT product.id AS product_id, product.key AS product_key,
  product.display_name AS product_name, product.entitlements_spec,
  price.id AS price_id, price.key AS price_key, price.amount AS unit_amount,
  price.currency, price.access_duration_hours, price.auto_renew
 FROM openrails.products product
 JOIN openrails.prices price ON price.product_id=product.id AND price.merchant_id=product.merchant_id
 WHERE product.merchant_id=sqlc.arg(merchant_id)::uuid
  AND (sqlc.narg(catalog_id)::uuid IS NULL OR product.catalog_id=sqlc.narg(catalog_id)::uuid)
  AND NOT product.archived AND NOT price.archived
  AND product.entitlements_spec ? wanted.entitlement
  AND ((sqlc.arg(kind)::text='permanent' AND NOT price.auto_renew AND price.access_duration_hours IS NULL AND COALESCE(product.entitlements_spec->>wanted.entitlement,'0')='0')
    OR (sqlc.arg(kind)::text='finite' AND NOT price.auto_renew AND price.access_duration_hours IS NOT NULL)
    OR (sqlc.arg(kind)::text='recurring' AND price.auto_renew))
  AND (wanted.after_id='00000000-0000-0000-0000-000000000000'::uuid OR
    (price.currency<>sqlc.arg(preferred_currency)::text, price.currency, price.id) >
    (wanted.after_currency<>sqlc.arg(preferred_currency)::text, wanted.after_currency, wanted.after_id))
 ORDER BY price.currency<>sqlc.arg(preferred_currency)::text, price.currency, price.id
 LIMIT sqlc.arg(page_limit)::int
) offer
ORDER BY wanted.entitlement, offer.currency<>sqlc.arg(preferred_currency)::text, offer.currency, offer.price_id;

-- A partial bundle remains useful; reject only when every durable benefit is
-- already owned or reserved by another accepted permanent purchase. Admission
-- calls this under the same customer lock used by session and sale insertion.
-- name: PermanentBenefitsCovered :one
SELECT cardinality(sqlc.arg(entitlements)::text[])>0 AND NOT EXISTS (
 SELECT 1 FROM unnest(sqlc.arg(entitlements)::text[]) AS wanted(key)
 WHERE NOT EXISTS (
   SELECT 1 FROM openrails.entitlements e
   WHERE e.merchant_id=sqlc.arg(merchant_id)::uuid AND e.customer_id=sqlc.arg(customer_id)::uuid
     AND e.entitlement=wanted.key AND e.end_at IS NULL AND e.start_at<=sqlc.arg(at_time)::timestamptz
     AND e.revoked_at IS NULL AND e.deleted_at IS NULL
 ) AND NOT (sqlc.arg(include_pending)::boolean AND (
   EXISTS (SELECT 1 FROM openrails.checkout_sessions s
    WHERE s.merchant_id=sqlc.arg(merchant_id)::uuid AND s.customer_id=sqlc.arg(customer_id)::uuid
      AND s.id<>sqlc.arg(except_session_id)::uuid AND s.mode='one_off' AND s.status<>'succeeded'
      AND (s.status IN ('created','requires_action') OR (s.rail IN ('stripe','solana') AND NOT COALESCE((s.rail_state->>'provider_closed')::boolean,false)))
      AND s.rail_state->'accepted_purchase'->>'access_duration_hours' IS NULL
      AND s.rail_state->'accepted_purchase'->'entitlements' ? wanted.key
      AND COALESCE(s.rail_state->'accepted_purchase'->'entitlements'->>wanted.key,'0')='0')
   OR EXISTS (SELECT 1 FROM openrails.rail_intents i
    WHERE i.merchant_id=sqlc.arg(merchant_id)::uuid AND i.intent_type='nmi_sale'
      AND i.payload->>'user_id'=sqlc.arg(customer_id)::uuid::text
      AND i.status IN ('pending','in_flight','unknown_needs_verify','failed_retryable')
      AND i.payload->>'access_duration_hours' IS NULL
      AND i.payload->'entitlements' ? wanted.key
      AND COALESCE(i.payload->'entitlements'->>wanted.key,'0')='0')
 ))
);
