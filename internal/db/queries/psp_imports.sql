-- name: CreatePSPImport :one
INSERT INTO billing.psp_imports (merchant_id, psp_id, rail, status, history_since, rules, products)
VALUES (sqlc.arg(merchant_id)::uuid, sqlc.arg(psp_id)::uuid, sqlc.arg(rail)::text, 'pulling', sqlc.narg(history_since)::timestamptz,
    sqlc.arg(rules)::jsonb, sqlc.arg(products)::jsonb)
RETURNING *;

-- name: GetPSPImport :one
SELECT * FROM billing.psp_imports WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid;

-- name: LockPSPImport :one
SELECT * FROM billing.psp_imports WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid FOR UPDATE;

-- name: ListPSPImportsPage :many
-- The merchant's imports, newest first.
SELECT * FROM billing.psp_imports
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND (sqlc.narg(after_at)::timestamptz IS NULL OR (created_at, id) < (sqlc.narg(after_at)::timestamptz, sqlc.narg(after_id)::uuid))
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg(row_limit)::int;

-- name: UpdatePSPImportConfig :one
UPDATE billing.psp_imports SET rules = sqlc.arg(rules)::jsonb, products = sqlc.arg(products)::jsonb, updated_at = now()
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid
RETURNING *;

-- name: SetPSPImportStatus :one
-- error is set exactly when the status is failed.
UPDATE billing.psp_imports SET status = sqlc.arg(status)::text, error = sqlc.narg(error)::text, updated_at = now()
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid
RETURNING *;

-- name: SetPSPImportRoster :exec
-- The roster was read at pulled_at; history then starts at history_since.
UPDATE billing.psp_imports SET pulled_at = sqlc.arg(pulled_at)::timestamptz, history_since = sqlc.narg(history_since)::timestamptz,
    history_through = sqlc.narg(history_since)::timestamptz, updated_at = now()
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid;

-- name: SetPSPImportHistoryThrough :exec
UPDATE billing.psp_imports SET history_through = sqlc.arg(history_through)::timestamptz, updated_at = now()
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid;

-- name: SetPSPImportApplied :exec
UPDATE billing.psp_imports SET status = 'applied', error = NULL, applied_at = sqlc.arg(applied_at)::timestamptz, updated_at = now()
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid;

-- name: DeletePSPImport :execrows
-- Hard delete: staged provider objects are working copies, gone with their import.
DELETE FROM billing.psp_imports WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid;

-- name: CountPSPImportRecords :many
SELECT kind, state, count(*)::int AS records FROM billing.psp_import_records
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND import_id = sqlc.arg(import_id)::uuid
GROUP BY kind, state
ORDER BY kind, state;

-- name: UpsertPSPImportRecords :exec
-- A re-pull refreshes what the provider says; links and outcomes are kept.
INSERT INTO billing.psp_import_records (merchant_id, import_id, kind, ref, customer_ref, subscription_ref, occurred_at, facts, data, state)
SELECT sqlc.arg(merchant_id)::uuid, sqlc.arg(import_id)::uuid, r.kind, r.ref, NULLIF(r.customer_ref, ''), NULLIF(r.subscription_ref, ''),
    NULLIF(r.occurred_at, '0001-01-01 00:00:00+00'::timestamptz), r.facts, r.data, 'unmatched'
FROM unnest(sqlc.arg(kinds)::text[], sqlc.arg(refs)::text[], sqlc.arg(customer_refs)::text[], sqlc.arg(subscription_refs)::text[],
    sqlc.arg(occurred_ats)::timestamptz[], sqlc.arg(facts)::jsonb[], sqlc.arg(data)::jsonb[])
    AS r(kind, ref, customer_ref, subscription_ref, occurred_at, facts, data)
ON CONFLICT (merchant_id, import_id, kind, ref) DO UPDATE SET customer_ref = EXCLUDED.customer_ref, subscription_ref = EXCLUDED.subscription_ref,
    occurred_at = EXCLUDED.occurred_at, facts = EXCLUDED.facts, data = EXCLUDED.data, updated_at = now();

-- name: ListPSPImportRecordsByKind :many
-- One kind's records in id order, for linking and attribution.
SELECT * FROM billing.psp_import_records
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND import_id = sqlc.arg(import_id)::uuid AND kind = sqlc.arg(kind)::text
  AND id > sqlc.arg(after_id)::uuid
ORDER BY id
LIMIT sqlc.arg(row_limit)::int;

-- name: ListPSPImportRecordsPage :many
SELECT * FROM billing.psp_import_records
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND import_id = sqlc.arg(import_id)::uuid
  AND (sqlc.narg(state)::text IS NULL OR state = sqlc.narg(state)::text)
  AND (sqlc.narg(kind)::text IS NULL OR kind = sqlc.narg(kind)::text)
  AND id > sqlc.arg(after_id)::uuid
ORDER BY id
LIMIT sqlc.arg(row_limit)::int;

-- name: SetPSPImportRecordLinks :exec
-- Rules decided these records; landed ones keep their outcome.
UPDATE billing.psp_import_records r
SET state = l.state, customer_id = NULLIF(l.customer_id, '00000000-0000-0000-0000-000000000000'::uuid), linked_via = NULLIF(l.linked_via, ''),
    reason = NULLIF(l.reason, ''), updated_at = now()
FROM unnest(sqlc.arg(ids)::uuid[], sqlc.arg(states)::text[], sqlc.arg(customer_ids)::uuid[], sqlc.arg(linked_vias)::text[], sqlc.arg(reasons)::text[])
    AS l(id, state, customer_id, linked_via, reason)
WHERE r.merchant_id = sqlc.arg(merchant_id)::uuid AND r.import_id = sqlc.arg(import_id)::uuid AND r.id = l.id AND r.state <> 'imported';

-- name: SetPSPImportRecordManualLinks :execrows
UPDATE billing.psp_import_records r
SET manual_customer_id = NULLIF(l.customer_id, '00000000-0000-0000-0000-000000000000'::uuid), updated_at = now()
FROM unnest(sqlc.arg(ids)::uuid[], sqlc.arg(customer_ids)::uuid[]) AS l(id, customer_id)
WHERE r.merchant_id = sqlc.arg(merchant_id)::uuid AND r.import_id = sqlc.arg(import_id)::uuid AND r.id = l.id;

-- name: ListPSPImportLinkedCustomers :many
-- The customers with records waiting to land, in order.
SELECT DISTINCT customer_id::uuid AS customer_id FROM billing.psp_import_records
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND import_id = sqlc.arg(import_id)::uuid AND state = 'linked'
  AND customer_id > sqlc.arg(after_customer_id)::uuid
ORDER BY 1
LIMIT sqlc.arg(row_limit)::int;

-- name: ListPSPImportCustomerRecords :many
-- Every linked or landed record of these customers: a book carries a customer whole.
SELECT * FROM billing.psp_import_records
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND import_id = sqlc.arg(import_id)::uuid
  AND customer_id = ANY(sqlc.arg(customer_ids)::uuid[]) AND state IN ('linked', 'imported')
ORDER BY customer_id, kind, id;

-- name: ListPSPImportRecordsByRef :many
SELECT * FROM billing.psp_import_records
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND import_id = sqlc.arg(import_id)::uuid AND kind = sqlc.arg(kind)::text
  AND ref = ANY(sqlc.arg(refs)::text[]);

-- name: SetPSPImportRecordOutcomes :exec
UPDATE billing.psp_import_records r SET state = o.state, reason = NULLIF(o.reason, ''), updated_at = now()
FROM unnest(sqlc.arg(ids)::uuid[], sqlc.arg(states)::text[], sqlc.arg(reasons)::text[]) AS o(id, state, reason)
WHERE r.merchant_id = sqlc.arg(merchant_id)::uuid AND r.import_id = sqlc.arg(import_id)::uuid AND r.id = o.id AND r.customer_id IS NOT NULL;

-- name: ListPSPImportPlans :many
-- The provider plans the linked subscriptions name.
SELECT DISTINCT data->>'plan' AS plan FROM billing.psp_import_records
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND import_id = sqlc.arg(import_id)::uuid AND kind = 'subscription' AND state = 'linked'
  AND COALESCE(data->>'plan', '') <> ''
ORDER BY 1;

-- name: ListPSPProviderPrices :many
-- The live provider prices mirroring these provider objects of one PSP.
SELECT COALESCE(b.plan_id, b.price_ref, b.recurring_billing_option_id)::text AS ref, price.public_id, price.amount, price.currency,
    price.billing_interval_hours, price.billing_interval_months
FROM billing.price_psp_bindings b
JOIN billing.prices price ON price.merchant_id = b.merchant_id AND price.id = b.price_id
WHERE b.merchant_id = sqlc.arg(merchant_id)::uuid AND b.psp_id = sqlc.arg(psp_id)::uuid AND price.provider_owned AND price.deleted_at IS NULL
  AND (b.plan_id = ANY(sqlc.arg(refs)::text[]) OR b.price_ref = ANY(sqlc.arg(refs)::text[]) OR b.recurring_billing_option_id = ANY(sqlc.arg(refs)::text[]));

-- name: UpsertPSPImportMappings :execrows
INSERT INTO billing.psp_import_mappings (merchant_id, mapping, key, customer_id)
SELECT sqlc.arg(merchant_id)::uuid, m.mapping, m.key, m.customer_id
FROM unnest(sqlc.arg(mappings)::text[], sqlc.arg(keys)::text[], sqlc.arg(customer_ids)::uuid[]) AS m(mapping, key, customer_id)
ON CONFLICT (merchant_id, mapping, key) DO UPDATE SET customer_id = EXCLUDED.customer_id, updated_at = now()
WHERE billing.psp_import_mappings.customer_id <> EXCLUDED.customer_id;

-- name: DeletePSPImportMappings :execrows
DELETE FROM billing.psp_import_mappings m
USING unnest(sqlc.arg(mappings)::text[], sqlc.arg(keys)::text[]) AS d(mapping, key)
WHERE m.merchant_id = sqlc.arg(merchant_id)::uuid AND m.mapping = d.mapping AND m.key = d.key;

-- name: LookupPSPImportMappings :many
SELECT key, customer_id FROM billing.psp_import_mappings
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND mapping = sqlc.arg(mapping)::text AND key = ANY(sqlc.arg(keys)::text[]);

-- name: ListPSPImportsByIDs :many
SELECT * FROM billing.psp_imports WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = ANY(sqlc.arg(ids)::uuid[])
ORDER BY created_at DESC, id DESC;

-- name: ListPSPImportRecordsByIDs :many
SELECT * FROM billing.psp_import_records
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND import_id = sqlc.arg(import_id)::uuid AND id = ANY(sqlc.arg(ids)::uuid[])
ORDER BY id;

-- name: ListProductProviderPrices :many
-- A product's provider prices on one PSP, for rails whose subscriptions name
-- no provider object.
SELECT price.public_id, price.amount, price.currency, price.billing_interval_hours, price.billing_interval_months
FROM billing.price_psp_bindings b
JOIN billing.prices price ON price.merchant_id = b.merchant_id AND price.id = b.price_id
JOIN billing.products prod ON prod.merchant_id = price.merchant_id AND prod.id = price.product_id
WHERE b.merchant_id = sqlc.arg(merchant_id)::uuid AND b.psp_id = sqlc.arg(psp_id)::uuid AND prod.key = sqlc.arg(product_key)::text
  AND price.provider_owned AND price.deleted_at IS NULL
ORDER BY price.public_id;
