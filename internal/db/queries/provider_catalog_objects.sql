-- name: UpsertProviderCatalogObjects :exec
-- What the provider says now; an older observation never replaces a newer one.
-- Empty strings, zero intervals and the zero time stand for NULL.
INSERT INTO billing.provider_catalog_objects (merchant_id, psp_id, kind, ref, product_ref, name, active, deleted_at, amount, currency,
    billing_interval_hours, billing_interval_months, evidence, observed_at, observed_via)
SELECT sqlc.arg(merchant_id)::uuid, sqlc.arg(psp_id)::uuid, o.kind, o.ref, NULLIF(o.product_ref, ''), NULLIF(o.name, ''), o.active,
    NULLIF(o.deleted_at, '0001-01-01 00:00:00+00'::timestamptz), NULLIF(o.amount, ''), NULLIF(o.currency, ''),
    NULLIF(o.hours, 0), NULLIF(o.months, 0), o.evidence, sqlc.arg(observed_at)::timestamptz, sqlc.arg(observed_via)::text
FROM unnest(sqlc.arg(kinds)::text[], sqlc.arg(refs)::text[], sqlc.arg(product_refs)::text[], sqlc.arg(names)::text[], sqlc.arg(actives)::boolean[],
    sqlc.arg(deleted_ats)::timestamptz[], sqlc.arg(amounts)::text[], sqlc.arg(currencies)::text[], sqlc.arg(hours)::int[], sqlc.arg(months)::int[],
    sqlc.arg(evidence)::jsonb[]) AS o(kind, ref, product_ref, name, active, deleted_at, amount, currency, hours, months, evidence)
ON CONFLICT (merchant_id, psp_id, kind, ref) DO UPDATE SET product_ref = EXCLUDED.product_ref, name = EXCLUDED.name, active = EXCLUDED.active,
    deleted_at = EXCLUDED.deleted_at, amount = EXCLUDED.amount, currency = EXCLUDED.currency, billing_interval_hours = EXCLUDED.billing_interval_hours,
    billing_interval_months = EXCLUDED.billing_interval_months, evidence = EXCLUDED.evidence, observed_at = EXCLUDED.observed_at,
    observed_via = EXCLUDED.observed_via, updated_at = now()
WHERE billing.provider_catalog_objects.observed_at <= EXCLUDED.observed_at;

-- name: MarkProviderCatalogObjectDeleted :execrows
-- A by-id read found the object gone; its last stated terms stay.
UPDATE billing.provider_catalog_objects
SET deleted_at = COALESCE(deleted_at, sqlc.arg(observed_at)::timestamptz), observed_at = sqlc.arg(observed_at)::timestamptz,
    observed_via = sqlc.arg(observed_via)::text, updated_at = now()
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND psp_id = sqlc.arg(psp_id)::uuid AND kind = sqlc.arg(kind)::text AND ref = sqlc.arg(ref)::text
  AND observed_at <= sqlc.arg(observed_at)::timestamptz;

-- name: GetProviderCatalogObject :one
-- One observed object; retired is the provider's not-for-sale.
SELECT o.*, (NOT o.active OR o.deleted_at IS NOT NULL OR COALESCE(NOT product.active OR product.deleted_at IS NOT NULL, false))::boolean AS retired
FROM billing.provider_catalog_objects o
LEFT JOIN billing.provider_catalog_objects product ON product.merchant_id = o.merchant_id AND product.psp_id = o.psp_id
    AND product.kind = 'product' AND product.ref = o.product_ref
WHERE o.merchant_id = sqlc.arg(merchant_id)::uuid AND o.psp_id = sqlc.arg(psp_id)::uuid AND o.kind = sqlc.arg(kind)::text AND o.ref = sqlc.arg(ref)::text;

-- name: ListProviderCatalogRefs :many
-- Every object observed on one PSP, for a pull to read again what its lists lack.
SELECT kind, ref FROM billing.provider_catalog_objects
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND psp_id = sqlc.arg(psp_id)::uuid
ORDER BY kind, ref;

-- name: ListProviderCatalog :many
-- One PSP's observed catalog in (kind, ref) order, with the live provider
-- price mapping each price (empty for none), the subscriptions parked on it
-- and its open unmapped finding. With import_id, only the prices that
-- import's staged subscriptions name, with how many name each.
SELECT o.kind, o.ref, o.product_ref, o.name, o.active, o.deleted_at, o.amount, o.currency, o.billing_interval_hours, o.billing_interval_months,
    o.observed_at, o.observed_via,
    (NOT o.active OR o.deleted_at IS NOT NULL OR COALESCE(NOT product.active OR product.deleted_at IS NOT NULL, false))::boolean AS retired,
    COALESCE(mapped.public_id, '')::text AS price, COALESCE(named.subscriptions, 0)::int AS subscriptions,
    (SELECT count(*) FROM billing.parked_provider_subscriptions pk WHERE pk.merchant_id = o.merchant_id AND pk.psp_id = o.psp_id
        AND pk.reason = 'price_unmapped' AND pk.price_ref = o.ref AND pk.released_at IS NULL)::int AS parked,
    COALESCE((SELECT f.id FROM billing.reconciliation_findings f WHERE f.merchant_id = o.merchant_id AND f.finding_type = 'catalog.price.unmapped'
        AND f.psp_id = o.psp_id AND f.subject_key = o.ref AND f.resolved_at IS NULL), '00000000-0000-0000-0000-000000000000'::uuid)::uuid AS finding
FROM billing.provider_catalog_objects o
LEFT JOIN billing.provider_catalog_objects product ON product.merchant_id = o.merchant_id AND product.psp_id = o.psp_id
    AND product.kind = 'product' AND product.ref = o.product_ref
LEFT JOIN billing.price_psp_bindings b ON o.kind = 'price' AND b.merchant_id = o.merchant_id AND b.psp_id = o.psp_id
    AND (b.plan_id = o.ref OR b.price_ref = o.ref)
LEFT JOIN billing.prices mapped ON mapped.merchant_id = b.merchant_id AND mapped.id = b.price_id AND mapped.provider_owned AND mapped.deleted_at IS NULL
LEFT JOIN (
    SELECT r.data->>'plan' AS plan, count(*) AS subscriptions FROM billing.psp_import_records r
    WHERE r.merchant_id = sqlc.arg(merchant_id)::uuid AND r.import_id = sqlc.narg(import_id)::uuid AND r.kind = 'subscription'
    GROUP BY 1
) named ON o.kind = 'price' AND named.plan = o.ref
WHERE o.merchant_id = sqlc.arg(merchant_id)::uuid AND o.psp_id = sqlc.arg(psp_id)::uuid
  AND (sqlc.narg(kind)::text IS NULL OR o.kind = sqlc.narg(kind)::text)
  AND (sqlc.narg(mapped)::boolean IS NULL OR (mapped.public_id IS NOT NULL) = sqlc.narg(mapped)::boolean)
  AND (sqlc.narg(retired)::boolean IS NULL
       OR (NOT o.active OR o.deleted_at IS NOT NULL OR COALESCE(NOT product.active OR product.deleted_at IS NOT NULL, false)) = sqlc.narg(retired)::boolean)
  AND (sqlc.narg(import_id)::uuid IS NULL OR named.subscriptions IS NOT NULL)
  AND (o.kind, o.ref) > (sqlc.arg(after_kind)::text, sqlc.arg(after_ref)::text)
ORDER BY o.kind, o.ref
LIMIT sqlc.arg(row_limit)::int;

-- name: ListPSPBoundProviderRefs :many
-- The NMI plans and Stripe prices that live provider prices of one PSP bind,
-- with the terms stored for each: a pull reads each again, and records a
-- vanished one never observed from these terms.
SELECT COALESCE(b.plan_id, b.price_ref)::text AS ref, price.amount, price.currency, price.billing_interval_hours, price.billing_interval_months
FROM billing.price_psp_bindings b
JOIN billing.prices price ON price.merchant_id = b.merchant_id AND price.id = b.price_id
WHERE b.merchant_id = sqlc.arg(merchant_id)::uuid AND b.psp_id = sqlc.arg(psp_id)::uuid AND price.provider_owned AND price.deleted_at IS NULL
  AND (b.plan_id IS NOT NULL OR b.price_ref IS NOT NULL)
ORDER BY 1;
