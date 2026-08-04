-- openrails.entitlements. The model is bun-soft-delete (deleted_at): every
-- read filters deleted_at IS NULL explicitly here (bun added it implicitly).

-- name: CreateEntitlement :one
INSERT INTO openrails.entitlements (
    id, merchant_id, customer_id, entitlement, start_at, end_at,
    source_id, source_type, grant_id, revoked_at, revoke_reason, created_at, updated_at
) VALUES (
    COALESCE(NULLIF(sqlc.arg(id)::uuid, '00000000-0000-0000-0000-000000000000'::uuid), uuidv7()),
    sqlc.arg(merchant_id)::uuid,
    NULLIF(sqlc.arg(customer_id)::uuid, '00000000-0000-0000-0000-000000000000'::uuid),
    $1, $2, sqlc.narg(end_at), sqlc.narg(source_id), $3, sqlc.narg(grant_id),
    sqlc.narg(revoked_at), sqlc.narg(revoke_reason),
    COALESCE(NULLIF(sqlc.arg(created_at)::timestamptz, '0001-01-01 00:00:00+00'::timestamptz), now()),
    COALESCE(NULLIF(sqlc.arg(updated_at)::timestamptz, '0001-01-01 00:00:00+00'::timestamptz), now())
)
RETURNING id;

-- name: EntitlementExistsActive :one
SELECT EXISTS (
    SELECT 1 FROM openrails.entitlements ent
    WHERE ent.merchant_id = $1
      AND ent.customer_id = $2
      AND ent.entitlement = $3
      AND ent.start_at <= sqlc.arg(at)::timestamptz
      AND (ent.end_at IS NULL OR ent.end_at > sqlc.arg(at)::timestamptz)
      AND ent.revoked_at IS NULL
      AND ent.deleted_at IS NULL
);

-- name: EntitlementHasActiveIndefinite :one
SELECT EXISTS (
    SELECT 1 FROM openrails.entitlements ent
    WHERE ent.merchant_id = $1
      AND ent.customer_id = $2
      AND ent.entitlement = $3
      AND ent.revoked_at IS NULL AND ent.end_at IS NULL
      AND ent.start_at <= sqlc.arg(at)::timestamptz
      AND ent.deleted_at IS NULL
);

-- name: GetLatestFiniteActiveEntitlement :one
SELECT * FROM openrails.entitlements ent
WHERE ent.merchant_id = $1
  AND ent.customer_id = $2
  AND ent.entitlement = $3
  AND ent.revoked_at IS NULL
  AND ent.deleted_at IS NULL
  AND ent.end_at IS NOT NULL
  AND ent.start_at <= sqlc.arg(at)::timestamptz
  AND ent.end_at > sqlc.arg(at)::timestamptz
ORDER BY ent.end_at DESC
LIMIT 1;

-- name: ListActiveEntitlementNames :many
-- No merchant_id predicate: matches the bun-era user-keyed variant exactly.
SELECT DISTINCT ent.entitlement FROM openrails.entitlements ent
WHERE ent.customer_id = $1
  AND ent.start_at <= sqlc.arg(at)::timestamptz
  AND (ent.end_at IS NULL OR ent.end_at > sqlc.arg(at)::timestamptz)
  AND ent.revoked_at IS NULL
  AND ent.deleted_at IS NULL;

-- name: ListActiveEntitlementNamesMerchant :many
SELECT DISTINCT ent.entitlement FROM openrails.entitlements ent
WHERE ent.merchant_id = $1
  AND ent.customer_id = $2
  AND ent.start_at <= sqlc.arg(at)::timestamptz
  AND (ent.end_at IS NULL OR ent.end_at > sqlc.arg(at)::timestamptz)
  AND ent.revoked_at IS NULL
  AND ent.deleted_at IS NULL;

-- name: ListCustomersWithEntitlement :many
-- Reverse lookup (#535): customer ids holding an ACTIVE window of `entitlement`,
-- keyset-paginated by customer_id (after_id is an exclusive lower bound — pass the
-- zero uuid to start). merchant scoping is RLS (no explicit merchant_id), matching
-- ListActiveEntitlementNames. Backs AuthKit's EntitlementFilterProvider (#91).
SELECT DISTINCT ent.customer_id FROM openrails.entitlements ent
WHERE ent.entitlement = sqlc.arg(entitlement)::text
  AND ent.start_at <= sqlc.arg(at)::timestamptz
  AND (ent.end_at IS NULL OR ent.end_at > sqlc.arg(at)::timestamptz)
  AND ent.revoked_at IS NULL
  AND ent.deleted_at IS NULL
  AND ent.customer_id > sqlc.arg(after_id)::uuid
ORDER BY ent.customer_id
LIMIT sqlc.arg(lim)::int;

-- name: ListActiveEntitlementRecords :many
SELECT * FROM openrails.entitlements ent
WHERE ent.customer_id = $1
  AND ent.revoked_at IS NULL
  AND ent.deleted_at IS NULL
  AND ent.start_at <= sqlc.arg(at)::timestamptz
  AND (ent.end_at IS NULL OR ent.end_at > sqlc.arg(at)::timestamptz)
ORDER BY ent.start_at ASC;

-- name: ListActiveEntitlementRecordsMerchant :many
SELECT * FROM openrails.entitlements ent
WHERE ent.merchant_id = $1
  AND ent.customer_id = $2
  AND ent.revoked_at IS NULL
  AND ent.deleted_at IS NULL
  AND ent.start_at <= sqlc.arg(at)::timestamptz
  AND (ent.end_at IS NULL OR ent.end_at > sqlc.arg(at)::timestamptz)
ORDER BY ent.start_at ASC;

-- name: ListDistinctEntitlementNamesBySource :many
SELECT DISTINCT ent.entitlement FROM openrails.entitlements ent
WHERE ent.source_type = $1
  AND ent.source_id = $2
  AND ent.revoked_at IS NULL
  AND ent.deleted_at IS NULL;

-- name: EndActiveEntitlementsBySubscription :exec
-- #691 closure write: bound a subscription's live windows to a PROVEN end
-- (user cancel at period end, terminal resolution). Advance-written on disk —
-- a dead system cannot extend a cancelled sub. start_at < end_at keeps the
-- generated period range valid; future-start windows are handled by
-- SoftDeleteFutureEntitlementsBySubscription.
UPDATE openrails.entitlements ent SET
    end_at = sqlc.arg(end_at)::timestamptz,
    updated_at = sqlc.arg(now)::timestamptz,
    revoked_at = CASE WHEN sqlc.arg(set_revoked)::boolean THEN sqlc.arg(now)::timestamptz ELSE ent.revoked_at END,
    revoke_reason = CASE WHEN sqlc.arg(set_revoked)::boolean THEN sqlc.narg(revoke_reason) ELSE ent.revoke_reason END
WHERE ent.source_type = 'subscription'
  AND ent.source_id = $1
  AND ent.revoked_at IS NULL
  AND ent.deleted_at IS NULL
  AND ent.start_at < sqlc.arg(end_at)::timestamptz
  AND (ent.end_at IS NULL OR ent.end_at > sqlc.arg(end_at)::timestamptz);

-- name: SoftDeleteFutureEntitlementsBySubscription :exec
-- #691 closure companion: scheduled windows starting at/after the proven end
-- cannot be bounded (end <= start); remove them.
UPDATE openrails.entitlements ent SET
    deleted_at = sqlc.arg(now)::timestamptz,
    updated_at = sqlc.arg(now)::timestamptz
WHERE ent.source_type = 'subscription'
  AND ent.source_id = $1
  AND ent.revoked_at IS NULL
  AND ent.deleted_at IS NULL
  AND ent.start_at >= sqlc.arg(end_at)::timestamptz;

-- name: ListExtendableSubscriptionEntitlements :many
SELECT * FROM openrails.entitlements ent
WHERE ent.source_type = 'subscription'
  AND ent.source_id = $1
  AND ent.revoked_at IS NULL
  AND ent.deleted_at IS NULL
  AND ent.end_at IS NOT NULL AND ent.end_at < sqlc.arg(end_at)::timestamptz
FOR UPDATE;

-- name: UpdateEntitlementEndAtIfMatch :exec
UPDATE openrails.entitlements ent SET
    end_at = sqlc.arg(new_end_at)::timestamptz,
    updated_at = sqlc.arg(now)::timestamptz
WHERE ent.id = $1
  AND ent.revoked_at IS NULL
  AND ent.deleted_at IS NULL
  AND ent.end_at = sqlc.arg(old_end_at)::timestamptz;

-- name: ResumeEntitlementsBySubscription :exec
-- #691 resume: re-open the LATEST live window per (customer, entitlement) of a
-- resumed auto-renew subscription (end_at = NULL), undoing an advance-written
-- cancel closure. Only the latest window per timeline (older bounded windows are
-- history), and only when no other live window would overlap [start, infinity)
-- — the GIST no-overlap constraint stays intact.
UPDATE openrails.entitlements ent SET
    end_at = NULL,
    updated_at = sqlc.arg(now)::timestamptz
WHERE ent.deleted_at IS NULL
  AND ent.id IN (
    SELECT DISTINCT ON (e.customer_id, e.entitlement) e.id
    FROM openrails.entitlements e
    WHERE e.source_type = 'subscription'
      AND e.source_id = $1
      AND e.revoked_at IS NULL
      AND e.deleted_at IS NULL
      AND e.end_at IS NOT NULL
      AND e.end_at > sqlc.arg(now)::timestamptz
      AND NOT EXISTS (
          SELECT 1 FROM openrails.entitlements o
          WHERE o.merchant_id = e.merchant_id
            AND o.customer_id = e.customer_id
            AND o.entitlement = e.entitlement
            AND o.id <> e.id
            AND o.revoked_at IS NULL
            AND o.deleted_at IS NULL
            AND o.period && tstzrange(e.start_at, 'infinity'::timestamptz, '[)')
      )
    ORDER BY e.customer_id, e.entitlement, e.end_at DESC
);

-- name: SoftDeleteFutureOneOffEntitlements :exec
UPDATE openrails.entitlements ent SET
    deleted_at = sqlc.arg(now)::timestamptz,
    updated_at = sqlc.arg(now)::timestamptz
WHERE ent.source_type = 'one_off'
  AND ent.source_id = $1
  AND ent.revoked_at IS NULL
  AND ent.deleted_at IS NULL
  AND ent.start_at >= sqlc.arg(end_at)::timestamptz;

-- name: RevokeActiveOneOffEntitlements :exec
UPDATE openrails.entitlements ent SET
    end_at = sqlc.arg(end_at)::timestamptz,
    revoked_at = sqlc.arg(now)::timestamptz,
    revoke_reason = sqlc.narg(revoke_reason),
    updated_at = sqlc.arg(now)::timestamptz
WHERE ent.source_type = 'one_off'
  AND ent.source_id = $1
  AND ent.revoked_at IS NULL
  AND ent.deleted_at IS NULL
  AND ent.start_at < sqlc.arg(end_at)::timestamptz
  AND (ent.end_at IS NULL OR ent.end_at > sqlc.arg(end_at)::timestamptz);

-- name: EntitlementExistsBySource :one
SELECT EXISTS (
    SELECT 1 FROM openrails.entitlements ent
    WHERE ent.source_type = $1
      AND ent.source_id = $2
      AND ent.entitlement = $3
      AND ent.revoked_at IS NULL
      AND ent.deleted_at IS NULL
);

-- name: ListEntitlementsByCustomer :many
SELECT * FROM openrails.entitlements ent
WHERE ent.customer_id = $1
  AND ent.deleted_at IS NULL
ORDER BY ent.start_at DESC;

-- name: GetEntitlementByID :one
SELECT * FROM openrails.entitlements ent
WHERE ent.id = $1
  AND ent.deleted_at IS NULL
LIMIT 1;

-- name: RevokeEntitlementByID :execrows
UPDATE openrails.entitlements ent SET
    revoked_at = sqlc.arg(now)::timestamptz,
    revoke_reason = sqlc.arg(revoke_reason)::text,
    updated_at = sqlc.arg(now)::timestamptz
WHERE ent.id = $1
  AND ent.revoked_at IS NULL
  AND ent.deleted_at IS NULL;

-- name: AcquireEntitlementTimelineLock :exec
-- Transaction-scoped advisory lock serializing timeline updates per
-- (subject, entitlement); the key is hashed in Go.
SELECT pg_advisory_xact_lock(sqlc.arg(key)::bigint);

-- name: ShiftEntitlementTimelineWindows :exec
UPDATE openrails.entitlements ent SET
    start_at = ent.start_at + (sqlc.arg(delta_seconds)::bigint * interval '1 second'),
    end_at = CASE WHEN ent.end_at IS NULL THEN NULL
             ELSE ent.end_at + (sqlc.arg(delta_seconds)::bigint * interval '1 second') END,
    updated_at = sqlc.arg(now)::timestamptz
WHERE ent.customer_id = $1
  AND ent.entitlement = $2
  AND ent.revoked_at IS NULL
  AND ent.deleted_at IS NULL
  AND ent.start_at >= sqlc.arg(from_at)::timestamptz
  AND NOT (ent.id = ANY(sqlc.arg(exclude_ids)::uuid[]));

-- name: TimelineHasIndefinite :one
SELECT EXISTS (
    SELECT 1 FROM openrails.entitlements ent
    WHERE ent.customer_id = $1
      AND ent.entitlement = $2
      AND ent.revoked_at IS NULL
      AND ent.deleted_at IS NULL
      AND ent.end_at IS NULL
);

-- name: GetTimelineIndefinite :one
SELECT * FROM openrails.entitlements ent
WHERE ent.customer_id = $1
  AND ent.entitlement = $2
  AND ent.revoked_at IS NULL
  AND ent.deleted_at IS NULL
  AND ent.end_at IS NULL
ORDER BY ent.start_at ASC
LIMIT 1;

-- name: GetTimelineTailEnd :one
-- The latest finite end on the timeline (the tail a new window starts after).
SELECT ent.end_at FROM openrails.entitlements ent
WHERE ent.customer_id = $1
  AND ent.entitlement = $2
  AND ent.revoked_at IS NULL
  AND ent.deleted_at IS NULL
  AND ent.end_at IS NOT NULL
ORDER BY ent.end_at DESC
LIMIT 1;

-- name: GetTimelineCoveringWindow :one
-- The window covering instant `at` (for already-covered EndAt requests).
SELECT * FROM openrails.entitlements ent
WHERE ent.customer_id = $1
  AND ent.entitlement = $2
  AND ent.revoked_at IS NULL
  AND ent.deleted_at IS NULL
  AND ent.start_at < sqlc.arg(at)::timestamptz
  AND (ent.end_at IS NULL OR ent.end_at >= sqlc.arg(at)::timestamptz)
ORDER BY ent.end_at DESC NULLS LAST
LIMIT 1;

-- name: AttachEntitlementCustomer :exec
-- Backfill the payable subject onto a legacy row that predates #317.
UPDATE openrails.entitlements ent SET
    customer_id = $2,
    updated_at = $3
WHERE ent.id = $1
  AND ent.customer_id IS NULL
  AND ent.deleted_at IS NULL;

-- name: SoftDeleteEntitlementByID :exec
UPDATE openrails.entitlements ent SET
    deleted_at = sqlc.arg(now)::timestamptz,
    updated_at = sqlc.arg(now)::timestamptz
WHERE ent.id = $1
  AND ent.revoked_at IS NULL
  AND ent.deleted_at IS NULL;

-- name: RevokeActiveTimelineWindows :exec
-- Revoke every currently-active window on the timeline, optionally filtered
-- to one source (NULL filter = any).
UPDATE openrails.entitlements ent SET
    revoked_at = sqlc.arg(now)::timestamptz,
    revoke_reason = sqlc.arg(revoke_reason)::text,
    updated_at = sqlc.arg(now)::timestamptz
WHERE ent.customer_id = $1
  AND ent.entitlement = $2
  AND ent.revoked_at IS NULL
  AND ent.deleted_at IS NULL
  AND ent.start_at <= sqlc.arg(now)::timestamptz
  AND (ent.end_at IS NULL OR ent.end_at > sqlc.arg(now)::timestamptz)
  AND (sqlc.narg(source_type)::text IS NULL OR ent.source_type = sqlc.narg(source_type)::text)
  AND (sqlc.narg(source_id)::uuid IS NULL OR ent.source_id = sqlc.narg(source_id)::uuid);

-- name: SoftDeleteFutureTimelineWindows :exec
-- Soft-delete every future scheduled window on the timeline, optionally
-- filtered to one source (NULL filter = any).
UPDATE openrails.entitlements ent SET
    deleted_at = sqlc.arg(now)::timestamptz,
    updated_at = sqlc.arg(now)::timestamptz
WHERE ent.customer_id = $1
  AND ent.entitlement = $2
  AND ent.revoked_at IS NULL
  AND ent.deleted_at IS NULL
  AND ent.start_at > sqlc.arg(now)::timestamptz
  AND (sqlc.narg(source_type)::text IS NULL OR ent.source_type = sqlc.narg(source_type)::text)
  AND (sqlc.narg(source_id)::uuid IS NULL OR ent.source_id = sqlc.narg(source_id)::uuid);

-- name: ListActiveEntitlementRecordsByCustomerIDs :many
-- Batch read (#354/#491): active entitlement rows for MANY payable customer ids
-- of one merchant in ONE query — the list-shaped consumers' answer to N
-- per-customer round trips. customers is UUID-only (#491): the caller derives the
-- customer ids (FederatedCustomerID / the subject UUID) and maps results back.
-- Customers with no active rows simply do not appear.
SELECT ent.* FROM openrails.entitlements ent
WHERE ent.merchant_id = sqlc.arg(merchant_id)
  AND ent.customer_id = ANY(sqlc.arg(customer_ids)::uuid[])
  AND ent.revoked_at IS NULL
  AND ent.deleted_at IS NULL
  AND ent.start_at <= sqlc.arg(at)::timestamptz
  AND (ent.end_at IS NULL OR ent.end_at > sqlc.arg(at)::timestamptz)
ORDER BY ent.customer_id, ent.start_at ASC;

-- name: GetEntitlementByGrant :one
-- #511 fetch-back: the entitlement window MaterializeGrant projected for a given
-- grant + feature (so PushNewEntitlement can return the created row).
SELECT * FROM openrails.entitlements
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND grant_id = sqlc.arg(grant_id)::uuid
  AND entitlement = sqlc.arg(entitlement)::text
  AND deleted_at IS NULL
ORDER BY created_at DESC
LIMIT 1;

-- name: StandingSubscriptionEntitlementExists :one
-- #691: is there a live STANDING (end_at IS NULL) window for this subscription
-- source? One standing window satisfies every per-period grant of the sub —
-- the derive-2 skip condition (mirrored by ListLiveGrantsMissingEffects).
SELECT EXISTS (
    SELECT 1 FROM openrails.entitlements e
    WHERE e.merchant_id = sqlc.arg(merchant_id)::uuid
      AND e.customer_id = sqlc.arg(customer_id)::uuid
      AND e.entitlement = sqlc.arg(entitlement)::text
      AND e.source_type = 'subscription'
      AND e.source_id = sqlc.arg(source_id)::uuid
      AND e.end_at IS NULL
      AND e.revoked_at IS NULL
      AND e.deleted_at IS NULL
) AS standing;
