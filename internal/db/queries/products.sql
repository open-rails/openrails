-- billing.products.

-- name: CreateProduct :execrows
INSERT INTO billing.products (
    id, merchant_id, key, display_name, description, credit_grant,
    tier_group, tier_rank, ownership, required_entitlements, archived, created_at, updated_at
) VALUES (
    $1,
    sqlc.arg(merchant_id)::uuid,
    $2, $3, sqlc.narg(description), sqlc.narg(credit_grant),
    NULLIF(sqlc.narg(tier_group)::text, ''),
    COALESCE(NULLIF(sqlc.arg(tier_rank)::int, 0), 0),
    sqlc.narg(ownership)::text,
    sqlc.arg(required_entitlements)::text[],
    sqlc.arg(archived)::boolean,
    COALESCE(NULLIF(sqlc.arg(created_at)::timestamptz, '0001-01-01 00:00:00+00'::timestamptz), now()),
    COALESCE(NULLIF(sqlc.arg(updated_at)::timestamptz, '0001-01-01 00:00:00+00'::timestamptz), now())
);

-- Reads leave deleted products out: nothing ever referred to them.

-- name: GetProductByID :one
SELECT * FROM billing.products WHERE products.merchant_id = sqlc.arg(merchant_id)::uuid AND id = $1 AND deleted_at IS NULL;

-- name: GetProductByKey :one
SELECT * FROM billing.products WHERE products.merchant_id = sqlc.arg(merchant_id)::uuid AND key = $1 AND deleted_at IS NULL;

-- name: ListProductsByIDs :many
SELECT * FROM billing.products WHERE products.merchant_id = sqlc.arg(merchant_id)::uuid AND id = ANY(sqlc.arg(ids)::uuid[]) AND deleted_at IS NULL
ORDER BY created_at DESC, id DESC;

-- name: ListActiveProducts :many
SELECT * FROM billing.products WHERE products.merchant_id = sqlc.arg(merchant_id)::uuid AND NOT archived;

-- name: ListAllProducts :many
SELECT * FROM billing.products
WHERE products.merchant_id = sqlc.arg(merchant_id)::uuid AND deleted_at IS NULL;

-- name: ListProductsFiltered :many
-- One keyset page, newest first: rows after (after_at, after_id).
SELECT * FROM billing.products
WHERE products.merchant_id = sqlc.arg(merchant_id)::uuid AND deleted_at IS NULL
  AND (sqlc.narg(archived)::boolean IS NULL OR archived = sqlc.narg(archived)::boolean)
  AND (sqlc.arg(tier_group)::text = '' OR lower(btrim(tier_group)) = lower(btrim(sqlc.arg(tier_group)::text)))
  AND (sqlc.narg(keys)::text[] IS NULL OR key = ANY (sqlc.narg(keys)::text[]))
  AND (sqlc.narg(entitlements)::text[] IS NULL OR id IN (
    SELECT pe.product_id FROM billing.product_entitlements pe
    WHERE pe.merchant_id = sqlc.arg(merchant_id)::uuid AND pe.entitlement = ANY (sqlc.narg(entitlements)::text[]) AND pe.removed_at IS NULL))
  -- For sale: some live price sells it new: one of ours, or a provider's
  -- that still sells (CCBill). A product without one is granted only.
  AND (sqlc.narg(for_sale)::boolean IS NULL OR sqlc.narg(for_sale)::boolean = EXISTS (
    SELECT 1 FROM billing.prices pr
    WHERE pr.merchant_id = products.merchant_id AND pr.product_id = products.id AND NOT pr.archived
      AND (NOT pr.provider_owned OR EXISTS (
        SELECT 1 FROM billing.price_psp_bindings b JOIN billing.psps s ON s.merchant_id = b.merchant_id AND s.id = b.psp_id
        WHERE b.merchant_id = pr.merchant_id AND b.price_id = pr.id AND s.rail = 'ccbill'))))
  AND (sqlc.narg(after_at)::timestamptz IS NULL OR (created_at, id) < (sqlc.narg(after_at)::timestamptz, sqlc.narg(after_id)::uuid))
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg(fetch_limit)::int;

-- name: PatchProduct :one
-- Every field is chosen at the write point, never copied from a stale read.
UPDATE billing.products SET
    display_name = COALESCE(sqlc.narg(display_name)::text, display_name),
    description = CASE WHEN sqlc.arg(set_description)::boolean THEN NULLIF(sqlc.narg(description)::text, '') ELSE description END,
    credit_grant = CASE WHEN sqlc.arg(set_credit_grant)::boolean THEN sqlc.narg(credit_grant)::jsonb ELSE credit_grant END,
    tier_group = CASE WHEN sqlc.arg(set_tier_group)::boolean THEN NULLIF(sqlc.narg(tier_group)::text, '') ELSE tier_group END,
    tier_rank = COALESCE(sqlc.narg(tier_rank)::int, tier_rank),
    ownership = CASE WHEN sqlc.arg(set_ownership)::boolean THEN sqlc.narg(ownership)::text ELSE ownership END,
    required_entitlements = CASE WHEN sqlc.arg(set_required_entitlements)::boolean THEN sqlc.arg(required_entitlements)::text[] ELSE required_entitlements END,
    archived = COALESCE(sqlc.narg(archived)::boolean, archived),
    revision = CASE WHEN sqlc.arg(keys_changed)::boolean THEN revision + 1 ELSE revision END,
    updated_at = now()
WHERE products.merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid AND deleted_at IS NULL
RETURNING *;

-- name: ReviveProduct :execrows
-- Declaring a deleted product's key again revives its row (the id is the
-- key's) with the new definition, as created now.
UPDATE billing.products SET
    display_name = sqlc.arg(display_name)::text,
    description = sqlc.narg(description)::text,
    credit_grant = sqlc.narg(credit_grant)::jsonb,
    tier_group = NULLIF(sqlc.narg(tier_group)::text, ''),
    tier_rank = sqlc.arg(tier_rank)::int,
    ownership = sqlc.narg(ownership)::text,
    required_entitlements = sqlc.arg(required_entitlements)::text[],
    archived = sqlc.arg(archived)::boolean,
    deleted_at = NULL,
    created_at = now(),
    updated_at = now()
WHERE products.merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid AND deleted_at IS NOT NULL;

-- name: LockProductForDelete :one
SELECT id, key FROM billing.products
WHERE products.merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid AND deleted_at IS NULL
FOR UPDATE;

-- name: ProductHoldsReferences :one
-- Whether a record refers to the product itself: a subscription, a grant,
-- product access, a price migration or a meter's rate card. Its prices'
-- references, purchase lines among them, are PricesHoldReferences'.
SELECT (
    EXISTS (SELECT 1 FROM billing.subscriptions WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND product_id = sqlc.arg(product_id)::uuid)
    OR EXISTS (SELECT 1 FROM billing.grants WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND product_id = sqlc.arg(product_id)::uuid)
    OR EXISTS (SELECT 1 FROM billing.product_access WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND product_id = sqlc.arg(product_id)::uuid)
    OR EXISTS (SELECT 1 FROM billing.price_migrations WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND from_product_id = sqlc.arg(product_id)::uuid)
    OR EXISTS (SELECT 1 FROM billing.catalog_rate_cards WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND product_id = sqlc.arg(product_id)::uuid)
)::boolean AS referenced;

-- name: DeleteProduct :execrows
-- Deletes a product nothing refers to; deleted is always archived.
UPDATE billing.products SET archived = true, deleted_at = now(), updated_at = now()
WHERE products.merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid AND deleted_at IS NULL;
