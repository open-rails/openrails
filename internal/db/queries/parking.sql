-- Parked provider subscriptions: live provider subscriptions converge cannot
-- land yet, and the PSP-scoped findings they raise.

-- name: LockParkCause :exec
-- Park and Wake serialize on one cause: whichever commits first, the other
-- sees it. The merchant row's key share is taken first: a catalog write
-- holds that row for update and then wakes, so a park must wait for it
-- before holding a cause, never after (its finding's key check would
-- deadlock).
SELECT pg_advisory_xact_lock(hashtextextended('openrails.park:' || m.id::text || ':' || sqlc.arg(psp_id)::uuid::text
    || ':' || sqlc.arg(reason)::text || ':' || sqlc.arg(ref)::text, 0))
FROM (SELECT id FROM billing.merchants WHERE id = sqlc.arg(merchant_id)::uuid FOR KEY SHARE) m;

-- name: GetOpenPark :one
SELECT * FROM billing.parked_provider_subscriptions
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND psp_id = sqlc.arg(psp_id)::uuid
  AND subscription_ref = sqlc.arg(subscription_ref)::text AND released_at IS NULL;

-- name: OpenPark :one
-- One open park per subscription: a park for another reason updates it in
-- place, so it mirrors the latest converge.
INSERT INTO billing.parked_provider_subscriptions (merchant_id, psp_id, rail, subscription_ref, reason, price_ref, customer_ref, import_owned, evidence, parked_at, last_seen_at)
VALUES (sqlc.arg(merchant_id)::uuid, sqlc.arg(psp_id)::uuid, sqlc.arg(rail)::text, sqlc.arg(subscription_ref)::text, sqlc.arg(reason)::text,
    sqlc.narg(price_ref)::text, sqlc.narg(customer_ref)::text, sqlc.arg(import_owned)::boolean, sqlc.arg(evidence)::jsonb, sqlc.arg(at)::timestamptz, sqlc.arg(at)::timestamptz)
ON CONFLICT (merchant_id, psp_id, subscription_ref) WHERE released_at IS NULL DO UPDATE SET
    reason = EXCLUDED.reason, price_ref = EXCLUDED.price_ref, customer_ref = EXCLUDED.customer_ref,
    import_owned = EXCLUDED.import_owned, evidence = EXCLUDED.evidence, last_seen_at = EXCLUDED.last_seen_at
RETURNING *;

-- name: CloseOpenPark :one
UPDATE billing.parked_provider_subscriptions SET released_at = sqlc.arg(at)::timestamptz, released_by = sqlc.arg(released_by)::text
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND psp_id = sqlc.arg(psp_id)::uuid
  AND subscription_ref = sqlc.arg(subscription_ref)::text AND released_at IS NULL
RETURNING *;

-- name: ListOpenParksByPrice :many
-- The price_unmapped parks a mapping of price_ref wakes, a page at a time.
SELECT subscription_ref, rail FROM billing.parked_provider_subscriptions
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND psp_id = sqlc.arg(psp_id)::uuid AND reason = 'price_unmapped'
  AND price_ref = sqlc.arg(price_ref)::text AND released_at IS NULL AND subscription_ref > sqlc.arg(after)::text
ORDER BY subscription_ref
LIMIT sqlc.arg(row_limit)::int;

-- name: ListOpenParksByCustomer :many
-- The customer_unlinked parks linking customer_ref wakes, a page at a time.
SELECT subscription_ref, rail FROM billing.parked_provider_subscriptions
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND psp_id = sqlc.arg(psp_id)::uuid AND reason = 'customer_unlinked'
  AND customer_ref = sqlc.arg(customer_ref)::text AND released_at IS NULL AND subscription_ref > sqlc.arg(after)::text
ORDER BY subscription_ref
LIMIT sqlc.arg(row_limit)::int;

-- name: CountOpenParksByPrice :one
-- The parks a price's finding counts: not those the import owns.
SELECT count(*) FROM billing.parked_provider_subscriptions
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND psp_id = sqlc.arg(psp_id)::uuid AND reason = 'price_unmapped'
  AND price_ref = sqlc.arg(price_ref)::text AND released_at IS NULL AND NOT import_owned;

-- name: SubscriptionLanded :one
-- Whether a live subscription row holds the provider subscription.
SELECT EXISTS (SELECT 1 FROM billing.subscriptions
    WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND psp_id = sqlc.arg(psp_id)::uuid
      AND rail_subscription_id = sqlc.arg(subscription_ref)::text AND deleted_at IS NULL)::boolean AS landed;

-- name: ProviderRefMapped :one
-- Whether a provider price or plan binds to a price of ours on this PSP.
SELECT (EXISTS (SELECT 1 FROM billing.price_psp_bindings b
        WHERE b.merchant_id = sqlc.arg(merchant_id)::uuid AND b.psp_id = sqlc.arg(psp_id)::uuid AND b.price_ref = sqlc.arg(ref)::text)
    OR EXISTS (SELECT 1 FROM billing.price_psp_bindings b
        WHERE b.merchant_id = sqlc.arg(merchant_id)::uuid AND b.psp_id = sqlc.arg(psp_id)::uuid AND b.plan_id = sqlc.arg(ref)::text))::boolean AS mapped;

-- name: ListVaultOwners :many
-- The customers holding live cards in one NMI vault; adoption needs exactly one.
SELECT DISTINCT customer_id FROM billing.payment_methods
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND psp_id = sqlc.arg(psp_id)::uuid AND custodian_id IS NULL
  AND rail_customer_ref = sqlc.arg(vault)::text AND status <> 'removed'
ORDER BY customer_id
LIMIT 2;

-- name: GetParkBacklog :one
-- The PSP's latest import that has not failed, and how it staged one
-- subscription: while it is pulling, or lists the subscription unlanded, the
-- import owns it and its park raises no finding. A failed import owns
-- nothing.
SELECT i.id AS import_id, i.status, COALESCE(r.state, '')::text AS staged_state
FROM billing.psp_imports i
LEFT JOIN billing.psp_import_records r ON r.merchant_id = i.merchant_id AND r.import_id = i.id
    AND r.kind = 'subscription' AND r.ref = sqlc.arg(subscription_ref)::text
WHERE i.merchant_id = sqlc.arg(merchant_id)::uuid AND i.psp_id = sqlc.arg(psp_id)::uuid
  AND i.status <> 'failed'
ORDER BY i.created_at DESC, i.id DESC
LIMIT 1;

-- name: GetProviderFinding :one
SELECT * FROM billing.reconciliation_findings
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND finding_type = sqlc.arg(finding_type)::text
  AND psp_id = sqlc.arg(psp_id)::uuid AND subject_key = sqlc.arg(subject_key)::text;

-- name: AutoResolveProviderFinding :execrows
-- AutoResolveFindingBySubject for a PSP-scoped finding: the same subject on
-- another PSP is another finding.
UPDATE billing.reconciliation_findings
SET status = 'fixed', resolution = 'auto_vanished', resolved_at = now(),
    notified_at = NULL, notified_severity = NULL, updated_at = now()
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND finding_type = sqlc.arg(finding_type)::text
  AND psp_id = sqlc.arg(psp_id)::uuid AND subject_key = sqlc.arg(subject_key)::text
  AND status IN ('reconcile_required', 'requires_review');

-- name: ListParkedSubscriptions :many
-- Newest first, with the subscription a park's landing created.
SELECT p.id, p.psp_id, p.rail, p.subscription_ref, p.reason, p.price_ref, p.customer_ref, p.import_owned, p.evidence, p.parked_at, p.last_seen_at,
    p.released_at, p.released_by, s.id AS subscription_id
FROM billing.parked_provider_subscriptions p
LEFT JOIN billing.subscriptions s ON s.merchant_id = p.merchant_id AND s.psp_id = p.psp_id
    AND s.rail_subscription_id = p.subscription_ref AND s.deleted_at IS NULL
WHERE p.merchant_id = sqlc.arg(merchant_id)::uuid
  AND (sqlc.narg(psp_id)::uuid IS NULL OR p.psp_id = sqlc.narg(psp_id)::uuid)
  AND (sqlc.narg(reason)::text IS NULL OR p.reason = sqlc.narg(reason)::text)
  AND (p.released_at IS NOT NULL) = sqlc.arg(released)::boolean
  AND (sqlc.narg(after_at)::timestamptz IS NULL OR (p.parked_at, p.id) < (sqlc.narg(after_at)::timestamptz, sqlc.narg(after_id)::uuid))
ORDER BY p.parked_at DESC, p.id DESC
LIMIT sqlc.arg(row_limit)::int;
