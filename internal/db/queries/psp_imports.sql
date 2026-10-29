-- name: CreatePSPImport :one
INSERT INTO billing.psp_imports (merchant_id, psp_id, rail, status, history_since, rules)
VALUES (sqlc.arg(merchant_id)::uuid, sqlc.arg(psp_id)::uuid, sqlc.arg(rail)::text, 'pulling', sqlc.narg(history_since)::timestamptz,
    sqlc.arg(rules)::jsonb)
RETURNING *;

-- name: GetPSPImport :one
SELECT * FROM billing.psp_imports WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid;

-- name: LockPSPImport :one
SELECT * FROM billing.psp_imports WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid FOR UPDATE;

-- name: ListPSPImportsPage :many
-- The merchant's imports, newest first.
SELECT * FROM billing.psp_imports
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND (sqlc.narg(psp_id)::uuid IS NULL OR psp_id = sqlc.narg(psp_id)::uuid)
  AND (sqlc.narg(after_at)::timestamptz IS NULL OR (created_at, id) < (sqlc.narg(after_at)::timestamptz, sqlc.narg(after_id)::uuid))
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg(row_limit)::int;

-- name: UpdatePSPImportRules :one
UPDATE billing.psp_imports SET rules = sqlc.arg(rules)::jsonb, updated_at = now()
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid
RETURNING *;

-- name: SetPSPImportStatus :one
-- error is set exactly when the status is failed; error_code only then.
UPDATE billing.psp_imports SET status = sqlc.arg(status)::text, error = sqlc.narg(error)::text, error_code = sqlc.narg(error_code)::text, updated_at = now()
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
UPDATE billing.psp_imports SET status = 'applied', error = NULL, error_code = NULL, applied_at = sqlc.arg(applied_at)::timestamptz, updated_at = now()
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
-- Every linked or landed record of these customers, and the provider
-- customers OpenRails holds for them (their cards' vaults): a book carries a
-- customer whole.
SELECT * FROM billing.psp_import_records
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND import_id = sqlc.arg(import_id)::uuid
  AND customer_id = ANY(sqlc.arg(customer_ids)::uuid[]) AND (state IN ('linked', 'imported') OR state = 'held' AND kind = 'customer')
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

-- name: ListPSPProviderPricesOfPSP :many
-- Every live provider price bound to one PSP, archived ones included, for
-- rails whose subscriptions name no provider object (CCBill).
SELECT price.id, price.public_id, price.amount, price.currency, price.billing_interval_hours, price.billing_interval_months
FROM billing.price_psp_bindings b
JOIN billing.prices price ON price.merchant_id = b.merchant_id AND price.id = b.price_id
WHERE b.merchant_id = sqlc.arg(merchant_id)::uuid AND b.psp_id = sqlc.arg(psp_id)::uuid AND price.provider_owned AND price.deleted_at IS NULL
ORDER BY price.public_id;

-- name: SetPSPImportRecordManualPrices :execrows
-- The prices the merchant picked by hand for subscriptions; the nil uuid clears one.
UPDATE billing.psp_import_records r
SET manual_price_id = NULLIF(l.price_id, '00000000-0000-0000-0000-000000000000'::uuid), updated_at = now()
FROM unnest(sqlc.arg(ids)::uuid[], sqlc.arg(price_ids)::uuid[]) AS l(id, price_id)
WHERE r.merchant_id = sqlc.arg(merchant_id)::uuid AND r.import_id = sqlc.arg(import_id)::uuid AND r.id = l.id AND r.kind = 'subscription';

-- name: ListCustomerVaultMethods :many
-- A customer's live cards in one NMI vault, newest first: what adopting a
-- subscription billing that vault bills.
SELECT rail_method_ref FROM billing.payment_methods
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND psp_id = sqlc.arg(psp_id)::uuid AND custodian_id IS NULL
  AND rail_customer_ref = sqlc.arg(vault)::text AND customer_id = sqlc.arg(customer_id)::uuid
  AND status = 'active' AND rail_method_ref IS NOT NULL
ORDER BY created_at DESC, id DESC
LIMIT 10;

-- name: GetLatestPSPImportRules :one
-- The rules of the PSP's latest import: a new import starts from them.
SELECT rules FROM billing.psp_imports
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND psp_id = sqlc.arg(psp_id)::uuid
ORDER BY created_at DESC, id DESC
LIMIT 1;

-- name: RequestPSPImportApply :one
-- A person asked to land the import; its corrections are approved in their name.
UPDATE billing.psp_imports SET status = 'applying', error = NULL, apply_requested_by = sqlc.arg(requested_by)::text, updated_at = now()
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid
RETURNING *;

-- name: RecordPSPImportBasis :exec
-- Every subscription of the PSP at its row version, before the roster read. A
-- retried pull keeps the first basis.
INSERT INTO billing.psp_import_basis (merchant_id, import_id, subscription_id, row_version)
SELECT s.merchant_id, sqlc.arg(import_id)::uuid, s.id, s.row_version FROM billing.subscriptions s
WHERE s.merchant_id = sqlc.arg(merchant_id)::uuid AND s.psp_id = sqlc.arg(psp_id)::uuid AND s.deleted_at IS NULL
ON CONFLICT DO NOTHING;

-- name: SetPSPImportBasis :exec
-- Subscriptions an import first finds held at its Apply, at their current row version.
INSERT INTO billing.psp_import_basis (merchant_id, import_id, subscription_id, row_version)
SELECT s.merchant_id, sqlc.arg(import_id)::uuid, s.id, s.row_version FROM billing.subscriptions s
WHERE s.merchant_id = sqlc.arg(merchant_id)::uuid AND s.id = ANY(sqlc.arg(subscription_ids)::uuid[])
ON CONFLICT (merchant_id, import_id, subscription_id) DO UPDATE SET row_version = EXCLUDED.row_version;

-- name: AddPSPImportBasis :exec
-- Subscriptions a relink first finds held that the pull's basis lacks, at
-- their current row version.
INSERT INTO billing.psp_import_basis (merchant_id, import_id, subscription_id, row_version)
SELECT s.merchant_id, sqlc.arg(import_id)::uuid, s.id, s.row_version FROM billing.subscriptions s
WHERE s.merchant_id = sqlc.arg(merchant_id)::uuid AND s.id = ANY(sqlc.arg(subscription_ids)::uuid[])
ON CONFLICT DO NOTHING;

-- name: GetPSPImportSuccessor :many
-- A later import of the same PSP that pulled: it supersedes this one.
SELECT n.id FROM billing.psp_imports n
WHERE n.merchant_id = sqlc.arg(merchant_id)::uuid AND n.psp_id = sqlc.arg(psp_id)::uuid
  AND n.created_at > sqlc.arg(created_at)::timestamptz AND n.status IN ('pulled', 'applied')
ORDER BY n.created_at LIMIT 1;

-- name: ListPSPImportBasis :many
SELECT subscription_id, row_version FROM billing.psp_import_basis
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND import_id = sqlc.arg(import_id)::uuid;

-- name: ListPSPHeldSubscriptions :many
-- The PSP's subscriptions OpenRails holds, by provider ref.
SELECT id, customer_id, rail_subscription_id::text AS ref, facts_authority, row_version, status FROM billing.subscriptions
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND psp_id = sqlc.arg(psp_id)::uuid AND deleted_at IS NULL
  AND rail_subscription_id = ANY(sqlc.arg(refs)::text[]);

-- name: ListPSPHeldVaults :many
-- The customers holding the PSP's provider customers, by provider ref.
SELECT DISTINCT rail_customer_ref::text AS ref, customer_id FROM billing.payment_methods
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND psp_id = sqlc.arg(psp_id)::uuid AND rail_customer_ref = ANY(sqlc.arg(refs)::text[])
UNION
SELECT remote_customer_ref, customer_id FROM billing.psp_customers
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND psp_id = sqlc.arg(psp_id)::uuid AND remote_customer_ref = ANY(sqlc.arg(refs)::text[])
ORDER BY 1, 2;

-- name: ListPSPHeldTransactions :many
-- The customers holding the PSP's provider transactions, by transaction id.
SELECT DISTINCT transaction_id::text AS ref, customer_id FROM billing.payments
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND psp_id = sqlc.arg(psp_id)::uuid AND deleted_at IS NULL
  AND transaction_id = ANY(sqlc.arg(refs)::text[])
ORDER BY 1, 2;

-- name: ListPSPImportProposeCustomers :many
-- The customers a reconcile diffs, in order: those holding the PSP's
-- subscriptions or cards, and the holders of the import's held records.
SELECT customer_id FROM (
    SELECT s.customer_id FROM billing.subscriptions s
    WHERE s.merchant_id = sqlc.arg(merchant_id)::uuid AND s.psp_id = sqlc.arg(psp_id)::uuid AND s.deleted_at IS NULL
    UNION
    SELECT pm.customer_id FROM billing.payment_methods pm
    WHERE pm.merchant_id = sqlc.arg(merchant_id)::uuid AND pm.psp_id = sqlc.arg(psp_id)::uuid
    UNION
    SELECT r.customer_id FROM billing.psp_import_records r
    WHERE r.merchant_id = sqlc.arg(merchant_id)::uuid AND r.import_id = sqlc.arg(import_id)::uuid AND r.state = 'held'
) c
WHERE customer_id > sqlc.arg(after_customer_id)::uuid
ORDER BY customer_id
LIMIT sqlc.arg(row_limit)::int;

-- name: ListPSPImportHeldRecords :many
-- The held records of these customers, and the conflicting records of
-- subscriptions OpenRails holds for them.
SELECT r.* FROM billing.psp_import_records r
WHERE r.merchant_id = sqlc.arg(merchant_id)::uuid AND r.import_id = sqlc.arg(import_id)::uuid
  AND (r.state = 'held' AND r.customer_id = ANY(sqlc.arg(customer_ids)::uuid[])
       OR r.state = 'conflict' AND r.kind = 'subscription' AND EXISTS (
           SELECT 1 FROM billing.subscriptions s JOIN billing.psp_imports i ON i.merchant_id = s.merchant_id AND i.psp_id = s.psp_id
           WHERE i.merchant_id = r.merchant_id AND i.id = r.import_id AND s.rail_subscription_id = r.ref
             AND s.customer_id = ANY(sqlc.arg(customer_ids)::uuid[]) AND s.deleted_at IS NULL))
ORDER BY r.kind, r.id;

-- name: CountPSPImportProposals :one
-- An import's proposals by outcome; corrections are the open ones its Apply approves.
SELECT count(*) FILTER (WHERE status IN ('reconcile_required', 'requires_review'))::int AS open,
    count(*) FILTER (WHERE status IN ('reconcile_required', 'requires_review') AND dismissed_at IS NULL AND (
        change->>'kind' IN ('record_charge', 'update_card')
        OR change->>'kind' = 'adopt_subscription' AND change->'to'->>'transition' IN ('renew', 'adopt_period_end')
        OR change->>'kind' = 'correct_subscription' AND NOT COALESCE((change->'effects'->>'ends_access')::boolean, false)))::int AS corrections,
    count(*) FILTER (WHERE resolution = 'approved')::int AS approved,
    count(*) FILTER (WHERE resolution = 'dismissed')::int AS dismissed,
    count(*) FILTER (WHERE resolution = 'superseded')::int AS superseded
FROM billing.reconciliation_findings
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND pull_id = sqlc.arg(pull_id)::uuid;

-- name: CountPSPLiveSubscriptions :one
-- The PSP's live provider subscriptions: what a roster read is weighed against.
SELECT count(*)::int FROM billing.subscriptions
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND psp_id = sqlc.arg(psp_id)::uuid AND deleted_at IS NULL
  AND rail_subscription_id IS NOT NULL AND status IN ('active', 'past_due', 'pending');

