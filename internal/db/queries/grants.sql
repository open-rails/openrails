-- The append-only grant ledger (billing.grants). derive-1 appends events here;
-- derive-2 folds them into product_access windows. entitlement and ownership
-- events are history; credit lots are credit_grants.

-- name: InsertGrant :one
INSERT INTO billing.grants (
    merchant_id, customer_id, product_id, kind, source_type, source_id, payment_id,
    event, supersedes_id, spec_snapshot, starts_at, ends_at, reason, actor, grant_reason, quantity
) VALUES (
    sqlc.arg(merchant_id)::uuid, sqlc.arg(customer_id)::uuid, sqlc.narg(product_id)::uuid,
    sqlc.arg(kind)::text, sqlc.arg(source_type)::text, NULLIF(sqlc.arg(source_id)::text, ''), sqlc.narg(payment_id)::uuid,
    sqlc.arg(event)::text, sqlc.narg(supersedes_id)::uuid, sqlc.narg(spec_snapshot)::jsonb,
    sqlc.arg(starts_at)::timestamptz, sqlc.narg(ends_at)::timestamptz, sqlc.narg(reason)::text,
    sqlc.narg(actor)::text, sqlc.narg(grant_reason)::text, sqlc.narg(quantity)::int
)
RETURNING *;

-- name: InsertAccessGrantOnce :one
-- An access grant at its natural key: a replay returns no row and the caller
-- reads the recorded grant.
INSERT INTO billing.grants (
    merchant_id, customer_id, product_id, kind, source_type, source_id, payment_id,
    event, starts_at, ends_at, reason, actor, grant_reason, quantity
) VALUES (
    sqlc.arg(merchant_id)::uuid, sqlc.arg(customer_id)::uuid, sqlc.arg(product_id)::uuid,
    'access', sqlc.arg(source_type)::text, sqlc.arg(source_id)::text, sqlc.narg(payment_id)::uuid,
    'grant', sqlc.arg(starts_at)::timestamptz, sqlc.narg(ends_at)::timestamptz,
    sqlc.narg(reason)::text, sqlc.narg(actor)::text, sqlc.narg(grant_reason)::text, sqlc.narg(quantity)::int
)
ON CONFLICT DO NOTHING
RETURNING *;

-- name: GetAccessGrantByIdempotencyKey :one
SELECT * FROM billing.grants
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND kind = 'access' AND event = 'grant'
  AND source_type = 'grant' AND source_id = sqlc.arg(idempotency_key)::text AND grant_reason <> 'migration'
  AND import_root_id IS NULL;

-- name: LockAccessGrantKey :exec
-- Requests under one product-access Idempotency-Key run one at a time.
SELECT pg_advisory_xact_lock(hashtextextended('access-grant-key:' || sqlc.arg(merchant_id)::uuid::text || ':' || sqlc.arg(idempotency_key)::text, 0));

-- name: LockImportedGrantSource :exec
SELECT pg_advisory_xact_lock(hashtextextended('imported-grant:' || sqlc.arg(merchant_id)::uuid::text || ':' || sqlc.arg(source_id)::text, 0));

-- name: GetLatestImportedGrant :one
SELECT * FROM billing.grants
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND source_type = 'grant'
  AND source_id = sqlc.arg(source_id)::text AND kind = 'access' AND event = 'grant'
  AND grant_reason = 'import' AND actor = 'import'
ORDER BY import_as_of DESC NULLS LAST
LIMIT 1;

-- name: InsertImportedGrant :one
INSERT INTO billing.grants (
    merchant_id, customer_id, product_id, kind, source_type, source_id, event,
    starts_at, ends_at, actor, grant_reason, import_as_of, import_root_id
) VALUES (
    sqlc.arg(merchant_id)::uuid, sqlc.arg(customer_id)::uuid, sqlc.arg(product_id)::uuid,
    'access', 'grant', sqlc.arg(source_id)::text, 'grant',
    sqlc.arg(starts_at)::timestamptz, sqlc.narg(ends_at)::timestamptz, 'import', 'import',
    sqlc.arg(import_as_of)::timestamptz, sqlc.narg(import_root_id)::uuid
)
ON CONFLICT DO NOTHING
RETURNING *;

-- name: GetGrantTermination :one
SELECT * FROM billing.grants
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND supersedes_id = sqlc.arg(grant_id)::uuid
  AND event IN ('revoke', 'expire', 'supersede');

-- name: GetAccessGrantByPurchase :one
SELECT * FROM billing.grants
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND kind = 'access' AND event = 'grant'
  AND source_type = 'purchase' AND payment_id = sqlc.arg(payment_id)::uuid AND product_id = sqlc.arg(product_id)::uuid;

-- name: ListLiveGrantsBySource :many
-- The live grants of one source: a subscription's periods and grace, a
-- purchase. Indexed by source; never the customer's whole history.
SELECT g.* FROM billing.grants g
WHERE g.merchant_id = sqlc.arg(merchant_id)::uuid AND g.customer_id = sqlc.arg(customer_id)::uuid
  AND g.kind = sqlc.arg(kind)::text AND g.event = 'grant'
  AND g.source_type = ANY(sqlc.arg(source_types)::text[]) AND g.source_id = sqlc.arg(source_id)::text
  AND NOT EXISTS (SELECT 1 FROM billing.grants t
      WHERE t.merchant_id = g.merchant_id AND t.supersedes_id = g.id AND t.event IN ('revoke', 'expire', 'supersede'))
ORDER BY g.id;

-- name: GetGrant :one
SELECT * FROM billing.grants
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid;

-- ListGrantsByCustomer: every grant-event for the customer (live or terminated),
-- the full input to a customer-scoped re-derive.
-- name: ListGrantsByCustomer :many
SELECT * FROM billing.grants
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND customer_id = sqlc.arg(customer_id)::uuid
  AND event = 'grant'
ORDER BY created_at;

-- name: IsGrantTerminated :one
SELECT EXISTS (
    SELECT 1 FROM billing.grants t
    WHERE t.merchant_id = sqlc.arg(merchant_id)::uuid
      AND t.supersedes_id = sqlc.arg(grant_id)::uuid
      AND t.event IN ('revoke', 'expire', 'supersede')
) AS terminated;

-- derive.grant.missing: succeeded, positive, non-subscription payments that
-- produced no grant and no credit lot (every purchase grants one). A
-- refunded purchase keeps its grant event, so it is not flagged. Surface-only:
-- re-granting re-runs derive-1. NULL customer_id = merchant-wide.
-- name: ListUngrantedGrantablePayments :many
SELECT p.id, p.customer_id, p.amount, p.currency
FROM billing.payments p
WHERE p.merchant_id = sqlc.arg(merchant_id)::uuid
  AND (sqlc.narg(customer_id)::uuid IS NULL OR p.customer_id = sqlc.narg(customer_id)::uuid)
  AND p.deleted_at IS NULL
  AND p.status = 'succeeded'
  AND p.amount > 0
  AND p.subscription_id IS NULL
  -- An invoice's lines name what they produced; a usage invoice's payment grants nothing.
  AND p.invoice_id IS NULL
  AND NOT EXISTS (
      SELECT 1 FROM billing.grants g
      WHERE g.merchant_id = p.merchant_id AND g.event = 'grant'
        AND (g.payment_id = p.id OR g.source_id = p.id::text)
  )
  AND NOT EXISTS (SELECT 1 FROM billing.credit_grants c WHERE c.merchant_id = p.merchant_id AND c.payment_id = p.id)
ORDER BY p.id
LIMIT sqlc.arg(row_limit)::int;

-- derive.grant.excess: live grants whose payment was refunded. Surface-only: a
-- goodwill refund may keep access, so an operator decides. NULL customer_id =
-- merchant-wide.
-- name: ListLiveGrantsWithRefundedPayment :many
SELECT g.id, g.customer_id, g.kind, g.payment_id
FROM billing.grants g
WHERE g.merchant_id = sqlc.arg(merchant_id)::uuid
  AND (sqlc.narg(customer_id)::uuid IS NULL OR g.customer_id = sqlc.narg(customer_id)::uuid)
  AND g.event = 'grant'
  AND g.payment_id IS NOT NULL
  AND NOT EXISTS (
      SELECT 1 FROM billing.grants tt
      WHERE tt.merchant_id = g.merchant_id AND tt.supersedes_id = g.id AND tt.event IN ('revoke', 'expire', 'supersede')
  )
  AND EXISTS (
      SELECT 1 FROM billing.payments p
      LEFT JOIN LATERAL (
          SELECT true AS settled FROM billing.payments r
          WHERE r.merchant_id = p.merchant_id AND r.refunded_payment_id = p.id
            AND r.deleted_at IS NULL AND r.status = 'succeeded'
            AND r.amount < 0 AND r.reversal_kind = 'refund'
          LIMIT 1) refund ON true
      WHERE p.id = g.payment_id AND p.merchant_id = g.merchant_id AND p.deleted_at IS NULL
        AND (p.status = 'refunded' OR refund.settled)
  )
ORDER BY g.id
LIMIT sqlc.arg(row_limit)::int;

-- derive.grant_effect.missing: live access grants without a window (revoked
-- windows count). NULL customer_id = merchant-wide. Repair = MaterializeGrant.
-- name: ListLiveGrantsMissingEffects :many
SELECT g.* FROM billing.grants g
WHERE g.merchant_id = sqlc.arg(merchant_id)::uuid
  AND (sqlc.narg(customer_id)::uuid IS NULL OR g.customer_id = sqlc.narg(customer_id)::uuid)
  AND g.event = 'grant'
  AND NOT EXISTS (
      SELECT 1 FROM billing.grants t
      WHERE t.merchant_id = g.merchant_id AND t.supersedes_id = g.id AND t.event IN ('revoke', 'expire', 'supersede')
  )
  AND NOT EXISTS (
      SELECT 1 FROM billing.subscriptions s
      WHERE g.kind = 'access' AND g.source_type = 'subscription'
        AND s.merchant_id = g.merchant_id AND s.customer_id = g.customer_id AND s.id::text = g.source_id
        AND s.deleted_at IS NULL AND s.status = 'canceled' AND s.cancel_type = 'chargeback'
        AND g.starts_at >= s.canceled_at
  )
  AND g.kind = 'access' AND NOT (g.import_root_id IS NOT NULL AND g.ends_at IS NOT NULL AND g.ends_at <= g.import_as_of)
  AND NOT EXISTS (
      SELECT 1 FROM billing.product_access pa
      WHERE pa.merchant_id = g.merchant_id AND pa.grant_id = g.id AND pa.deleted_at IS NULL)
ORDER BY g.created_at, g.id
LIMIT sqlc.arg(row_limit)::int;

-- name: AccessGrantPastChargebackBoundary :one
-- Recheck after the customer lock: a missing-effect finding may predate the
-- source's chargeback. Its gated subscription repair owns the termination.
SELECT EXISTS (
    SELECT 1 FROM billing.grants g
    JOIN billing.subscriptions s ON s.merchant_id = g.merchant_id
      AND s.customer_id = g.customer_id AND s.id::text = g.source_id
    WHERE g.merchant_id = sqlc.arg(merchant_id)::uuid AND g.id = sqlc.arg(grant_id)::uuid
      AND g.kind = 'access' AND g.event = 'grant' AND g.source_type = 'subscription'
      AND s.deleted_at IS NULL AND s.status = 'canceled' AND s.cancel_type = 'chargeback'
      AND g.starts_at >= s.canceled_at
      AND NOT EXISTS (
          SELECT 1 FROM billing.grants t
          WHERE t.merchant_id = g.merchant_id AND t.supersedes_id = g.id
            AND t.event IN ('revoke', 'expire', 'supersede')
      )
) AS beyond_boundary;

-- derive.grant_effect.excess: terminated access grants whose window is still
-- unrevoked. NULL customer_id = merchant-wide. Repair = MaterializeGrant,
-- idempotent.
-- name: ListUnretractedTerminations :many
SELECT g.* FROM billing.grants g
WHERE g.merchant_id = sqlc.arg(merchant_id)::uuid
  AND (sqlc.narg(customer_id)::uuid IS NULL OR g.customer_id = sqlc.narg(customer_id)::uuid)
  AND g.event = 'grant'
  AND EXISTS (
      SELECT 1 FROM billing.grants t
      WHERE t.merchant_id = g.merchant_id AND t.supersedes_id = g.id
        AND t.event IN ('revoke', 'expire', 'supersede')
  )
  AND g.kind = 'access' AND EXISTS (
      SELECT 1 FROM billing.product_access pa
      WHERE pa.merchant_id = g.merchant_id AND pa.grant_id = g.id
        AND pa.revoked_at IS NULL AND pa.deleted_at IS NULL
        AND NOT EXISTS (
            SELECT 1 FROM billing.grants t
            WHERE t.merchant_id = g.merchant_id AND t.supersedes_id = g.id
              AND t.event IN ('revoke', 'supersede') AND t.actor = 'import'
              AND g.source_type = 'grant' AND g.grant_reason = 'import' AND g.actor = 'import'
              AND pa.ends_at <= t.starts_at))
ORDER BY g.created_at, g.id
LIMIT sqlc.arg(row_limit)::int;

-- derive.subscription.missing: subscriptions in an access-granting state with no
-- subscription-sourced grant; derive-1 materializes the grant and its window
-- (computed Go-side). A chargeback cancel grants no runway. scan_since skips
-- windows that ended long ago. A provider-default window ends at the paid
-- period end; dunning access is grace's. NULL customer_id = merchant-wide.
-- name: ListUngrantedSubscriptions :many
SELECT s.id, s.customer_id, s.product_id, s.status,
       s.current_period_starts_at, s.current_period_ends_at,
       s.started_at, s.ended_at, s.access_duration_hours_snapshot, s.provider_access_default_snapshot
FROM billing.subscriptions s
WHERE s.merchant_id = sqlc.arg(merchant_id)::uuid
  AND (sqlc.narg(customer_id)::uuid IS NULL OR s.customer_id = sqlc.narg(customer_id)::uuid)
  AND s.deleted_at IS NULL
  -- Engine card access is authored only by its atomic accepted-payment writer.
  AND NOT (s.collection_policy='engine' AND s.rail IN ('nmi','stripe'))
  AND (s.status IN ('active', 'canceled', 'unverified', 'awaiting_method') OR (s.status = 'past_due' AND s.collection_policy <> 'engine'))
  AND NOT (s.status = 'canceled' AND s.cancel_type = 'chargeback')
  AND (s.provider_access_default_snapshot AND s.current_period_ends_at >= sqlc.arg(scan_since)::timestamptz
       OR NOT s.provider_access_default_snapshot AND (s.access_duration_hours_snapshot IS NULL OR
       COALESCE(s.current_period_starts_at, s.started_at) + s.access_duration_hours_snapshot * interval '1 hour' >= sqlc.arg(scan_since)::timestamptz))
  AND NOT EXISTS (
      SELECT 1 FROM billing.grants g
      WHERE g.merchant_id = s.merchant_id AND g.event = 'grant'
        AND g.source_type = 'subscription' AND g.source_id = s.id::text
  )
ORDER BY COALESCE(s.current_period_starts_at, s.started_at), s.id
LIMIT sqlc.arg(row_limit)::int;

-- derive.wallet.missing: succeeded Solana wallet payments with a stored access
-- window (metadata.expiration_rfc3339) and no grant. The stored expiry makes the
-- window [purchased_at, expiration) unambiguous, so unlike derive.grant.missing
-- this auto-repairs.
-- name: ListUngrantedWalletPayments :many
SELECT DISTINCT p.id, p.customer_id, p.purchased_at,
       (p.metadata->>'expiration_rfc3339')::timestamptz AS expires_at, l.product_id
FROM billing.payments p
JOIN billing.invoice_lines l ON l.invoice_id = p.invoice_id AND l.merchant_id = p.merchant_id AND l.product_id IS NOT NULL
WHERE p.merchant_id = sqlc.arg(merchant_id)::uuid
  AND (sqlc.narg(customer_id)::uuid IS NULL OR p.customer_id = sqlc.narg(customer_id)::uuid)
  AND p.deleted_at IS NULL
  AND p.rail = 'solana'
  AND p.status = 'succeeded'
  AND p.amount > 0
  AND p.subscription_id IS NULL
  AND p.metadata->>'expiration_rfc3339' IS NOT NULL
  AND p.metadata->>'expiration_rfc3339' ~ '^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}'
  AND (p.metadata->>'expiration_rfc3339')::timestamptz > p.purchased_at
  AND (p.metadata->>'expiration_rfc3339')::timestamptz >= sqlc.arg(scan_since)::timestamptz
  AND NOT EXISTS (
      SELECT 1 FROM billing.grants g
      WHERE g.merchant_id = p.merchant_id AND g.event = 'grant'
        AND ((g.source_type = 'purchase' AND g.source_id = p.id::text) OR g.payment_id = p.id)
  )
  AND NOT EXISTS (SELECT 1 FROM billing.credit_grants c WHERE c.merchant_id = p.merchant_id AND c.payment_id = p.id)
ORDER BY p.purchased_at, p.id
LIMIT sqlc.arg(row_limit)::int;

-- An access window is a distinct fact even when it overlaps another paid
-- window or has no expiry. A replay must not reinstate a revoked grant.
-- name: AccessGrantWindowExists :one
SELECT EXISTS (
    SELECT 1 FROM billing.grants g
    WHERE g.merchant_id = sqlc.arg(merchant_id)::uuid
      AND g.customer_id = sqlc.arg(customer_id)::uuid
      AND g.product_id = sqlc.arg(product_id)::uuid
      AND g.kind = 'access' AND g.event = 'grant'
      AND g.source_type = sqlc.arg(source_type)::text
      AND g.source_id = sqlc.arg(source_id)::text
      AND g.starts_at = sqlc.arg(starts_at)::timestamptz
      AND g.ends_at IS NOT DISTINCT FROM sqlc.narg(ends_at)::timestamptz
) AS exists;

-- name: ListInitialMembershipGrants :many
-- Original source events before the accepted initial period ends; later renewal
-- events and later revocations do not rewrite this initial history.
SELECT * FROM billing.grants
WHERE merchant_id=sqlc.arg(merchant_id)::uuid AND source_type='subscription'
  AND source_id=sqlc.arg(subscription_id)::uuid::text AND event='grant'
  AND starts_at < sqlc.arg(before)::timestamptz
ORDER BY id LIMIT sqlc.arg(row_limit)::int;

-- name: HasInitialMembershipGrant :one
-- Refused or still-pending initial membership cannot own a grant at any instant.
SELECT EXISTS(SELECT 1 FROM billing.grants
WHERE merchant_id=sqlc.arg(merchant_id)::uuid AND source_type='subscription'
  AND source_id=sqlc.arg(subscription_id)::uuid::text AND event='grant')::boolean;

-- name: ListRenewalGrantsForArchive :many
SELECT * FROM billing.grants
WHERE merchant_id=sqlc.arg(merchant_id)::uuid AND source_type='subscription'
  AND source_id=sqlc.arg(subscription_id)::uuid::text AND event='grant'
  AND starts_at=sqlc.arg(period_start)::timestamptz
ORDER BY id LIMIT sqlc.arg(row_limit)::int;

-- name: CountUnpaidEngineRenewalGrants :one
-- Initial and pre-engine history precedes the first accepted engine period.
-- Every later source grant needs its own successful accepted period, including
-- grants following a declined attempt whose later retry bought the same window.
SELECT count(*) FROM billing.grants g
WHERE g.merchant_id=sqlc.arg(merchant_id)::uuid AND g.source_type='subscription'
  AND g.event='grant'
  AND EXISTS (SELECT 1 FROM billing.provider_intents i
    WHERE i.merchant_id=g.merchant_id AND i.intent_type='subscription_collection'
      AND i.subscription_id::text=g.source_id
      AND g.starts_at >= (i.payload->'renewal'->>'period_start')::timestamptz)
  AND NOT EXISTS (SELECT 1 FROM billing.provider_intents i
    WHERE i.merchant_id=g.merchant_id AND i.intent_type='subscription_collection'
      AND i.subscription_id::text=g.source_id AND i.status='succeeded'
      AND g.starts_at=(i.payload->'renewal'->>'period_start')::timestamptz
      AND g.ends_at IS NOT DISTINCT FROM
          (i.payload->'renewal'->>'period_start')::timestamptz +
          (i.payload->'renewal'->>'access_duration_hours')::int * interval '1 hour');


-- name: ListAccessGrantsAt :many
-- The access grant of one source period, read back after a concurrent replay.
SELECT * FROM billing.grants
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND customer_id = sqlc.arg(customer_id)::uuid
  AND product_id = sqlc.arg(product_id)::uuid AND kind = 'access' AND event = 'grant'
  AND source_type = sqlc.arg(source_type)::text AND source_id = sqlc.arg(source_id)::text
  AND starts_at = sqlc.arg(starts_at)::timestamptz
ORDER BY id;
