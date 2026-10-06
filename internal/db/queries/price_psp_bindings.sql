-- name: ListPricePSPBindings :many
SELECT b.*, p.rail, p.key AS psp_key
FROM billing.price_psp_bindings b
JOIN billing.psps p ON p.id = b.psp_id AND p.merchant_id = b.merchant_id
WHERE b.merchant_id = sqlc.arg(merchant_id)::uuid AND (cardinality(sqlc.arg(price_ids)::uuid[]) = 0 OR b.price_id = ANY(sqlc.arg(price_ids)::uuid[]))
  AND (sqlc.narg(psp_id)::uuid IS NULL OR b.psp_id = sqlc.narg(psp_id)::uuid)
ORDER BY b.price_id, b.psp_id;

-- name: ResolvePriceBindingPSP :many
SELECT * FROM billing.psps
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND rail = sqlc.arg(rail)::text
  AND ((sqlc.narg(psp_id)::uuid IS NOT NULL AND id = sqlc.narg(psp_id)::uuid)
       OR (sqlc.narg(psp_id)::uuid IS NULL AND key = sqlc.arg(psp_key)::text));

-- name: DeletePricePSPBindings :exec
DELETE FROM billing.price_psp_bindings
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND price_id = sqlc.arg(price_id)::uuid;

-- name: InsertPricePSPBinding :exec
INSERT INTO billing.price_psp_bindings
(merchant_id, price_id, psp_id, plan_id, price_ref, recurring_billing_option_id, plan_pda, flex_id, configuration)
SELECT sqlc.arg(merchant_id)::uuid, sqlc.arg(price_id)::uuid, sqlc.arg(psp_id)::uuid,
    sqlc.narg(plan_id), sqlc.narg(price_ref), sqlc.narg(recurring_billing_option_id), sqlc.narg(plan_pda), sqlc.narg(flex_id), sqlc.arg(configuration)::jsonb
FROM billing.prices owned_price
WHERE owned_price.merchant_id=sqlc.arg(merchant_id)::uuid AND owned_price.id=sqlc.arg(price_id)::uuid;

-- name: LockPriceForBindingUpdate :one
SELECT id FROM billing.prices
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(price_id)::uuid
FOR UPDATE;
