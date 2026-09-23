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
-- name: ListOffersForEntitlement :many
SELECT product.id AS product_id, product.key AS product_key,
 product.display_name AS product_name, product.entitlements_spec,
 price.id AS price_id, price.key AS price_key, price.amount AS unit_amount,
 price.currency, price.access_duration_hours, price.auto_renew
FROM openrails.products product
JOIN openrails.prices price ON price.product_id=product.id AND price.merchant_id=product.merchant_id
WHERE product.merchant_id=sqlc.arg(merchant_id)::uuid
 AND (sqlc.narg(catalog_id)::uuid IS NULL OR product.catalog_id=sqlc.narg(catalog_id)::uuid)
 AND NOT product.archived AND NOT price.archived
 AND product.entitlements_spec ? sqlc.arg(entitlement)::text
 AND ((sqlc.arg(kind)::text='permanent' AND NOT price.auto_renew AND price.access_duration_hours IS NULL)
   OR (sqlc.arg(kind)::text='finite' AND NOT price.auto_renew AND price.access_duration_hours IS NOT NULL)
   OR (sqlc.arg(kind)::text='recurring' AND price.auto_renew))
 AND (sqlc.narg(after_id)::uuid IS NULL OR
   (price.currency<>sqlc.arg(preferred_currency)::text, price.currency, price.id) >
   (sqlc.arg(after_currency)::text<>sqlc.arg(preferred_currency)::text, sqlc.arg(after_currency)::text, sqlc.narg(after_id)::uuid))
ORDER BY price.currency<>sqlc.arg(preferred_currency)::text, price.currency, price.id
LIMIT sqlc.arg(page_limit)::int;
