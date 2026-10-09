-- Discovery only: all pricing and benefits are revalidated at admission.
-- One page per requested key; uuid.Nil in after_ids starts a key's first page.
-- name: ListOffersForEntitlements :many
SELECT wanted.entitlement::text AS entitlement, offer.product_id, offer.product_key,
 offer.product_name, offer.price_id, offer.price_key,
 offer.unit_amount, offer.currency, offer.access_duration_hours, offer.billing_interval_hours
FROM unnest(sqlc.arg(entitlements)::text[], sqlc.arg(after_currencies)::text[], sqlc.arg(after_ids)::uuid[])
 AS wanted(entitlement, after_currency, after_id)
CROSS JOIN LATERAL (
 SELECT product.id AS product_id, product.key AS product_key,
  product.display_name AS product_name,
  price.id AS price_id, price.key AS price_key, price.amount AS unit_amount,
  price.currency, price.access_duration_hours, price.billing_interval_hours
 FROM billing.products product
 JOIN billing.prices price ON price.product_id=product.id AND price.merchant_id=product.merchant_id
 WHERE product.merchant_id=sqlc.arg(merchant_id)::uuid
  AND NOT product.archived AND NOT price.archived
  AND EXISTS (SELECT 1 FROM billing.product_entitlements pe
   WHERE pe.merchant_id = product.merchant_id AND pe.product_id = product.id
     AND pe.entitlement = wanted.entitlement AND pe.removed_at IS NULL)
  AND ((sqlc.arg(kind)::text='permanent' AND price.billing_interval_hours IS NULL AND price.access_duration_hours IS NULL)
    OR (sqlc.arg(kind)::text='finite' AND price.billing_interval_hours IS NULL AND price.access_duration_hours IS NOT NULL)
    OR (sqlc.arg(kind)::text='recurring' AND price.billing_interval_hours IS NOT NULL))
  AND (wanted.after_id='00000000-0000-0000-0000-000000000000'::uuid OR
    (price.currency<>sqlc.arg(preferred_currency)::text, price.currency, price.id) >
    (wanted.after_currency<>sqlc.arg(preferred_currency)::text, wanted.after_currency, wanted.after_id))
 ORDER BY price.currency<>sqlc.arg(preferred_currency)::text, price.currency, price.id
 LIMIT sqlc.arg(page_limit)::int
) offer
ORDER BY wanted.entitlement, offer.currency<>sqlc.arg(preferred_currency)::text, offer.currency, offer.price_id;
