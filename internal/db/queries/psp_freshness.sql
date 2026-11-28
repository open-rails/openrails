-- Known-through per PSP: the evidence of each of the merchant's PSPs. A PSP
-- with no stale episode is complete through now; a stale one through its
-- latest evidence.
-- name: ListPSPFreshness :many
SELECT p.id AS psp_id, p.key, p.rail, p.superseded_at,
       f.read_at, f.failing_since, f.failed_at, f.failure, f.imported_through, f.stale_since, f.notified_at, f.caught_up_at, f.catch_up,
       w.last_accepted_at, w.last_pull_at,
       EXISTS (SELECT 1 FROM billing.subscriptions s
                WHERE s.merchant_id = p.merchant_id AND s.psp_id = p.id AND s.deleted_at IS NULL
                  AND s.collection_policy <> 'engine' AND s.status IN ('active', 'past_due', 'awaiting_method', 'unverified'))::boolean AS live_subscriptions,
       EXISTS (SELECT 1 FROM billing.reconciliation_findings r
                WHERE r.merchant_id = p.merchant_id AND r.finding_type = 'life.webhooks.silent' AND r.subject_key = 'psp:' || p.id::text
                  AND r.status IN ('requires_review', 'reconcile_required'))::boolean AS webhooks_silent,
       (EXISTS (SELECT 1 FROM billing.subscriptions s WHERE s.merchant_id = p.merchant_id AND s.psp_id = p.id AND s.deleted_at IS NULL)
        OR EXISTS (SELECT 1 FROM billing.payments y WHERE y.merchant_id = p.merchant_id AND y.psp_id = p.id AND y.deleted_at IS NULL))::boolean AS has_data
FROM billing.psps p
LEFT JOIN billing.psp_freshness f ON f.merchant_id = p.merchant_id AND f.psp_id = p.id
LEFT JOIN billing.webhook_health w ON w.merchant_id = p.merchant_id AND w.psp_id = p.id
WHERE p.merchant_id = sqlc.arg(merchant_id)::uuid
  AND (sqlc.narg(psp_id)::uuid IS NULL OR p.id = sqlc.narg(psp_id)::uuid)
ORDER BY p.created_at, p.id;

-- A provider read of the PSP succeeded at read_at: its failure clears. While
-- stale, the record stays complete only through what it was: the catch-up
-- that reads the gap moves it. Returns the stale episode, NULL while current.
-- name: RecordPSPReadSucceeded :one
INSERT INTO billing.psp_freshness (merchant_id, psp_id, read_at)
VALUES (sqlc.arg(merchant_id)::uuid, sqlc.arg(psp_id)::uuid, sqlc.arg(read_at)::timestamptz)
ON CONFLICT (merchant_id, psp_id) DO UPDATE SET
    read_at = CASE WHEN psp_freshness.stale_since IS NULL THEN greatest(psp_freshness.read_at, EXCLUDED.read_at) ELSE psp_freshness.read_at END,
    failing_since = NULL, failed_at = NULL, failure = NULL, updated_at = now()
RETURNING stale_since;

-- A provider read of the PSP failed at failed_at: the failure is recorded and
-- a stale episode opens, unless one is open.
-- name: RecordPSPReadFailed :one
INSERT INTO billing.psp_freshness (merchant_id, psp_id, failing_since, failed_at, failure, stale_since)
VALUES (sqlc.arg(merchant_id)::uuid, sqlc.arg(psp_id)::uuid, sqlc.arg(failed_at)::timestamptz, sqlc.arg(failed_at)::timestamptz, sqlc.arg(failure)::text, sqlc.arg(failed_at)::timestamptz)
ON CONFLICT (merchant_id, psp_id) DO UPDATE SET
    failing_since = COALESCE(psp_freshness.failing_since, EXCLUDED.failing_since),
    failed_at = greatest(psp_freshness.failed_at, EXCLUDED.failed_at),
    failure = EXCLUDED.failure,
    stale_since = COALESCE(psp_freshness.stale_since, EXCLUDED.stale_since),
    updated_at = now()
RETURNING stale_since;

-- A declared book naming the PSP landed what it held as of imported_through.
-- name: RecordPSPImported :exec
INSERT INTO billing.psp_freshness (merchant_id, psp_id, imported_through)
VALUES (sqlc.arg(merchant_id)::uuid, sqlc.arg(psp_id)::uuid, sqlc.arg(imported_through)::timestamptz)
ON CONFLICT (merchant_id, psp_id) DO UPDATE SET
    imported_through = greatest(psp_freshness.imported_through, EXCLUDED.imported_through),
    updated_at = now();

-- name: LockPSPFreshness :one
SELECT * FROM billing.psp_freshness
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND psp_id = sqlc.arg(psp_id)::uuid
FOR UPDATE;

-- The stale episode's notice is queued once.
-- name: MarkPSPStaleNotified :execrows
UPDATE billing.psp_freshness SET notified_at = sqlc.arg(notified_at)::timestamptz, updated_at = now()
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND psp_id = sqlc.arg(psp_id)::uuid
  AND stale_since = sqlc.arg(stale_since)::timestamptz AND notified_at IS NULL;

-- A catch-up read the gap through through: the stale episode ends.
-- name: CompletePSPCatchUp :execrows
UPDATE billing.psp_freshness
SET stale_since = NULL, notified_at = NULL, failing_since = NULL, failed_at = NULL, failure = NULL,
    read_at = greatest(read_at, sqlc.arg(through)::timestamptz),
    caught_up_at = sqlc.arg(through)::timestamptz, catch_up = sqlc.arg(catch_up)::jsonb, updated_at = now()
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND psp_id = sqlc.arg(psp_id)::uuid
  AND stale_since = sqlc.arg(stale_since)::timestamptz;

-- CROSS-MERCHANT: the PSPs in a stale episode due to be tried again: none
-- failed since retry_before. A page after the cursor.
-- name: ListStalePSPs :many
SELECT merchant_id, psp_id FROM billing.psp_freshness
WHERE stale_since IS NOT NULL
  AND (failed_at IS NULL OR failed_at <= sqlc.arg(retry_before)::timestamptz)
  AND (merchant_id, psp_id) > (sqlc.arg(after_merchant_id)::uuid, sqlc.arg(after_psp_id)::uuid)
ORDER BY merchant_id, psp_id
LIMIT sqlc.arg(row_limit)::int;

-- What a catch-up reads: the PSP's provider-billed subscriptions the overdue
-- detector skipped while it was stale (a renewal past its rail's deadline with
-- no payment since) and its unverified ones, oldest period first, a page
-- after the (period end, id) cursor.
-- name: ListCatchUpSubscriptions :many
SELECT s.id, s.current_period_ends_at
FROM billing.subscriptions s
WHERE s.merchant_id = sqlc.arg(merchant_id)::uuid AND s.psp_id = sqlc.arg(psp_id)::uuid
  AND s.deleted_at IS NULL AND s.collection_policy <> 'engine' AND s.rail_subscription_id IS NOT NULL
  AND (s.status = 'unverified'
       OR (s.status = 'active'
           AND ((s.rail = 'nmi' AND s.current_period_ends_at < sqlc.arg(nmi_overdue_before)::timestamptz)
                OR (s.rail <> 'nmi' AND s.current_period_ends_at < sqlc.arg(overdue_before)::timestamptz))
           AND NOT EXISTS (
               SELECT 1 FROM billing.payments p
               WHERE p.merchant_id = s.merchant_id AND p.subscription_id = s.id
                 AND p.deleted_at IS NULL AND p.status = 'succeeded'
                 AND p.purchased_at >= s.current_period_ends_at)))
  AND (COALESCE(s.current_period_ends_at, '0001-01-01 00:00:00+00'::timestamptz), s.id) > (sqlc.arg(after_due_at)::timestamptz, sqlc.arg(after_id)::uuid)
ORDER BY COALESCE(s.current_period_ends_at, '0001-01-01 00:00:00+00'::timestamptz), s.id
LIMIT sqlc.arg(row_limit)::int;

-- What the gap from..through hid, as the catch-up's reads recorded it: the
-- PSP's renewals paid and failed in it, its subscriptions canceled in it, and
-- those still unconfirmed.
-- name: PSPCatchUpSummary :one
SELECT
    (SELECT count(*) FROM billing.payments p
      WHERE p.merchant_id = sqlc.arg(merchant_id)::uuid AND p.psp_id = sqlc.arg(psp_id)::uuid AND p.subscription_id IS NOT NULL
        AND p.deleted_at IS NULL AND p.status = 'succeeded'
        AND p.purchased_at > sqlc.arg(from_at)::timestamptz AND p.purchased_at <= sqlc.arg(through)::timestamptz)::bigint AS renewed,
    (SELECT count(*) FROM billing.payment_attempts a
      WHERE a.merchant_id = sqlc.arg(merchant_id)::uuid AND a.psp_id = sqlc.arg(psp_id)::uuid AND a.subscription_id IS NOT NULL
        AND a.outcome = 'declined'
        AND a.attempted_at > sqlc.arg(from_at)::timestamptz AND a.attempted_at <= sqlc.arg(through)::timestamptz)::bigint AS failed,
    (SELECT count(*) FROM billing.subscriptions s
      WHERE s.merchant_id = sqlc.arg(merchant_id)::uuid AND s.psp_id = sqlc.arg(psp_id)::uuid AND s.deleted_at IS NULL
        AND s.status = 'canceled' AND s.canceled_at > sqlc.arg(from_at)::timestamptz AND s.canceled_at <= sqlc.arg(through)::timestamptz)::bigint AS canceled,
    (SELECT count(*) FROM billing.subscriptions s
      WHERE s.merchant_id = sqlc.arg(merchant_id)::uuid AND s.psp_id = sqlc.arg(psp_id)::uuid AND s.deleted_at IS NULL
        AND s.status = 'unverified')::bigint AS unconfirmed;

-- A read of the provider that could not tell: the unverified rows it read
-- are unconfirmed, no longer awaiting it.
-- name: MarkSubscriptionsUnconfirmed :execrows
UPDATE billing.subscriptions
SET unverified_reason = 'unconfirmed', updated_at = sqlc.arg(read_at)::timestamptz
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = ANY(sqlc.arg(ids)::uuid[])
  AND status = 'unverified' AND unverified_reason = 'awaiting_provider' AND deleted_at IS NULL;

-- name: EnqueuePSPHostEvent :exec
INSERT INTO billing.host_outbox (merchant_id, event_type, subject_type, subject_id, occurred_at, data, dedupe_key)
VALUES (sqlc.arg(merchant_id)::uuid, sqlc.arg(event_type)::text, 'psp', sqlc.arg(psp_id)::uuid, sqlc.arg(occurred_at)::timestamptz, sqlc.arg(data)::jsonb, sqlc.arg(dedupe_key)::text)
ON CONFLICT (merchant_id, dedupe_key) DO NOTHING;

-- What awaits each PSP at now, per currency: its subscriptions unverified
-- awaiting it, and, while it is stale, its active ones whose renewal came due.
-- name: ListPSPAwaiting :many
SELECT s.psp_id, m.currency, count(*)::bigint AS subscriptions, COALESCE(sum(m.mrr), 0)::bigint AS mrr
FROM billing.subscriptions s
JOIN billing.subscription_mrr m ON m.merchant_id = s.merchant_id AND m.subscription_id = s.id
WHERE s.merchant_id = sqlc.arg(merchant_id)::uuid AND s.deleted_at IS NULL AND s.collection_policy <> 'engine' AND m.mrr > 0
  AND ((s.status = 'unverified' AND s.unverified_reason = 'awaiting_provider')
       OR (s.status = 'active' AND s.current_period_ends_at <= sqlc.arg(now)::timestamptz
           AND EXISTS (SELECT 1 FROM billing.psp_freshness f WHERE f.merchant_id = s.merchant_id AND f.psp_id = s.psp_id AND f.stale_since IS NOT NULL)))
GROUP BY s.psp_id, m.currency
ORDER BY s.psp_id, m.currency;
