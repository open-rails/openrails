-- Resolve opaque product keys and current ownership in one bounded query.
-- Archived products remain readable when a retained purchase grants access.
-- name: CheckProductAccessKeys :many
SELECT candidate.product_key::text AS product_key, p.id AS product_id, EXISTS (
 SELECT 1 FROM openrails.grants g
 WHERE g.merchant_id = sqlc.arg(merchant_id)::uuid
   AND g.customer_id = sqlc.arg(customer_id)::uuid
   AND g.product_id = p.id
   AND g.kind = 'ownership' AND g.event = 'grant'
   AND g.starts_at <= sqlc.arg(at_time)::timestamptz
   AND (g.ends_at IS NULL OR g.ends_at > sqlc.arg(at_time)::timestamptz)
   AND NOT EXISTS (SELECT 1 FROM openrails.grants t
    WHERE t.merchant_id = g.merchant_id AND t.supersedes_id = g.id
      AND t.event IN ('revoke','expire','supersede'))
) AS has_access
FROM unnest(sqlc.arg(product_keys)::text[]) AS candidate(product_key)
LEFT JOIN openrails.products p ON p.merchant_id = sqlc.arg(merchant_id)::uuid
 AND p.key = candidate.product_key
 AND (sqlc.narg(catalog_id)::uuid IS NULL OR p.catalog_id = sqlc.narg(catalog_id)::uuid);
