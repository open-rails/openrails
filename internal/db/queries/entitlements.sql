-- billing.entitlements. The model is bun-soft-delete (deleted_at): every
-- read filters deleted_at IS NULL explicitly here (bun added it implicitly).

-- name: CreateEntitlement :one
INSERT INTO billing.entitlements (
    id, tenant_id, tenant_subject_id, entitlement, start_at, end_at,
    source_id, source_type, revoked_at, revoke_reason, created_at, updated_at
) VALUES (
    COALESCE(NULLIF(sqlc.arg(id)::uuid, '00000000-0000-0000-0000-000000000000'::uuid), uuidv7()),
    COALESCE(NULLIF(sqlc.arg(tenant_id)::uuid, '00000000-0000-0000-0000-000000000000'::uuid),
             '00000000-0000-0000-0000-000000000001'::uuid),
    NULLIF(sqlc.arg(tenant_subject_id)::uuid, '00000000-0000-0000-0000-000000000000'::uuid),
    $1, $2, sqlc.narg(end_at), sqlc.narg(source_id), $3,
    sqlc.narg(revoked_at), sqlc.narg(revoke_reason),
    COALESCE(NULLIF(sqlc.arg(created_at)::timestamptz, '0001-01-01 00:00:00+00'::timestamptz), now()),
    COALESCE(NULLIF(sqlc.arg(updated_at)::timestamptz, '0001-01-01 00:00:00+00'::timestamptz), now())
)
RETURNING id;

-- name: EntitlementExistsActive :one
SELECT EXISTS (
    SELECT 1 FROM billing.entitlements ent
    WHERE ent.tenant_id = $1
      AND ent.tenant_subject_id = $2
      AND ent.entitlement = $3
      AND ent.start_at <= sqlc.arg(at)::timestamptz
      AND (ent.end_at IS NULL OR ent.end_at > sqlc.arg(at)::timestamptz)
      AND ent.revoked_at IS NULL
      AND ent.deleted_at IS NULL
);

-- name: EntitlementHasActiveIndefinite :one
SELECT EXISTS (
    SELECT 1 FROM billing.entitlements ent
    WHERE ent.tenant_id = $1
      AND ent.tenant_subject_id = $2
      AND ent.entitlement = $3
      AND ent.revoked_at IS NULL AND ent.end_at IS NULL
      AND ent.start_at <= sqlc.arg(at)::timestamptz
      AND ent.deleted_at IS NULL
);

-- name: GetLatestActiveEntitlement :one
SELECT * FROM billing.entitlements ent
WHERE ent.tenant_id = $1
  AND ent.tenant_subject_id = $2
  AND ent.entitlement = $3
  AND ent.revoked_at IS NULL
  AND ent.deleted_at IS NULL
ORDER BY ent.start_at DESC
LIMIT 1;

-- name: GetLatestFiniteActiveEntitlement :one
SELECT * FROM billing.entitlements ent
WHERE ent.tenant_id = $1
  AND ent.tenant_subject_id = $2
  AND ent.entitlement = $3
  AND ent.revoked_at IS NULL
  AND ent.deleted_at IS NULL
  AND ent.end_at IS NOT NULL
  AND ent.start_at <= sqlc.arg(at)::timestamptz
  AND ent.end_at > sqlc.arg(at)::timestamptz
ORDER BY ent.end_at DESC
LIMIT 1;

-- name: ListActiveEntitlementNames :many
-- No tenant_id predicate: matches the bun-era user-keyed variant exactly.
SELECT DISTINCT ent.entitlement FROM billing.entitlements ent
WHERE ent.tenant_subject_id = $1
  AND ent.start_at <= sqlc.arg(at)::timestamptz
  AND (ent.end_at IS NULL OR ent.end_at > sqlc.arg(at)::timestamptz)
  AND ent.revoked_at IS NULL
  AND ent.deleted_at IS NULL;

-- name: ListActiveEntitlementNamesTenant :many
SELECT DISTINCT ent.entitlement FROM billing.entitlements ent
WHERE ent.tenant_id = $1
  AND ent.tenant_subject_id = $2
  AND ent.start_at <= sqlc.arg(at)::timestamptz
  AND (ent.end_at IS NULL OR ent.end_at > sqlc.arg(at)::timestamptz)
  AND ent.revoked_at IS NULL
  AND ent.deleted_at IS NULL;

-- name: ListActiveEntitlementRecords :many
SELECT * FROM billing.entitlements ent
WHERE ent.tenant_subject_id = $1
  AND ent.revoked_at IS NULL
  AND ent.deleted_at IS NULL
  AND ent.start_at <= sqlc.arg(at)::timestamptz
  AND (ent.end_at IS NULL OR ent.end_at > sqlc.arg(at)::timestamptz)
ORDER BY ent.start_at ASC;

-- name: ListActiveEntitlementRecordsTenant :many
SELECT * FROM billing.entitlements ent
WHERE ent.tenant_id = $1
  AND ent.tenant_subject_id = $2
  AND ent.revoked_at IS NULL
  AND ent.deleted_at IS NULL
  AND ent.start_at <= sqlc.arg(at)::timestamptz
  AND (ent.end_at IS NULL OR ent.end_at > sqlc.arg(at)::timestamptz)
ORDER BY ent.start_at ASC;

-- name: ListDistinctEntitlementNamesBySource :many
SELECT DISTINCT ent.entitlement FROM billing.entitlements ent
WHERE ent.source_type = $1
  AND ent.source_id = $2
  AND ent.revoked_at IS NULL
  AND ent.deleted_at IS NULL;

-- name: CountInvalidEndBySubscription :one
SELECT count(*) FROM billing.entitlements ent
WHERE ent.source_type = 'subscription'
  AND ent.source_id = $1
  AND ent.revoked_at IS NULL
  AND ent.deleted_at IS NULL
  AND (ent.end_at IS NULL OR ent.end_at > sqlc.arg(end_at)::timestamptz)
  AND ent.start_at >= sqlc.arg(end_at)::timestamptz;

-- name: EndActiveEntitlementsBySubscription :exec
UPDATE billing.entitlements ent SET
    end_at = sqlc.arg(end_at)::timestamptz,
    updated_at = sqlc.arg(now)::timestamptz,
    revoked_at = CASE WHEN sqlc.arg(set_revoked)::boolean THEN sqlc.arg(now)::timestamptz ELSE ent.revoked_at END,
    revoke_reason = CASE WHEN sqlc.arg(set_revoked)::boolean THEN sqlc.narg(revoke_reason) ELSE ent.revoke_reason END
WHERE ent.source_type = 'subscription'
  AND ent.source_id = $1
  AND ent.revoked_at IS NULL
  AND ent.deleted_at IS NULL
  AND (ent.end_at IS NULL OR ent.end_at > sqlc.arg(end_at)::timestamptz);

-- name: ListExtendableSubscriptionEntitlements :many
SELECT * FROM billing.entitlements ent
WHERE ent.source_type = 'subscription'
  AND ent.source_id = $1
  AND ent.revoked_at IS NULL
  AND ent.deleted_at IS NULL
  AND ent.end_at IS NOT NULL AND ent.end_at < sqlc.arg(end_at)::timestamptz
FOR UPDATE;

-- name: UpdateEntitlementEndAtIfMatch :exec
UPDATE billing.entitlements ent SET
    end_at = sqlc.arg(new_end_at)::timestamptz,
    updated_at = sqlc.arg(now)::timestamptz
WHERE ent.id = $1
  AND ent.revoked_at IS NULL
  AND ent.deleted_at IS NULL
  AND ent.end_at = sqlc.arg(old_end_at)::timestamptz;

-- name: ResumeEntitlementsBySubscription :exec
UPDATE billing.entitlements ent SET
    end_at = NULL,
    updated_at = sqlc.arg(now)::timestamptz
WHERE ent.source_type = 'subscription'
  AND ent.source_id = $1
  AND ent.revoked_at IS NULL
  AND ent.deleted_at IS NULL
  AND ent.end_at IS NOT NULL
  AND ent.end_at > sqlc.arg(now)::timestamptz;

-- name: SoftDeleteFutureOneOffEntitlements :exec
UPDATE billing.entitlements ent SET
    deleted_at = sqlc.arg(now)::timestamptz,
    updated_at = sqlc.arg(now)::timestamptz
WHERE ent.source_type = 'one_off'
  AND ent.source_id = $1
  AND ent.revoked_at IS NULL
  AND ent.deleted_at IS NULL
  AND ent.start_at >= sqlc.arg(end_at)::timestamptz;

-- name: RevokeActiveOneOffEntitlements :exec
UPDATE billing.entitlements ent SET
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
    SELECT 1 FROM billing.entitlements ent
    WHERE ent.source_type = $1
      AND ent.source_id = $2
      AND ent.entitlement = $3
      AND ent.revoked_at IS NULL
      AND ent.deleted_at IS NULL
);

-- name: ListEntitlementsByTenantSubject :many
SELECT * FROM billing.entitlements ent
WHERE ent.tenant_subject_id = $1
  AND ent.deleted_at IS NULL
ORDER BY ent.start_at DESC;

-- name: GetEntitlementByID :one
SELECT * FROM billing.entitlements ent
WHERE ent.id = $1
  AND ent.deleted_at IS NULL
LIMIT 1;

-- name: RevokeEntitlementByID :execrows
UPDATE billing.entitlements ent SET
    revoked_at = sqlc.arg(now)::timestamptz,
    revoke_reason = sqlc.arg(revoke_reason)::text,
    updated_at = sqlc.arg(now)::timestamptz
WHERE ent.id = $1
  AND ent.revoked_at IS NULL
  AND ent.deleted_at IS NULL;

-- name: RevokeEntitlementBySubscriptionAndName :execrows
UPDATE billing.entitlements ent SET
    revoked_at = sqlc.arg(revoke_at)::timestamptz,
    revoke_reason = sqlc.arg(revoke_reason)::text,
    end_at = sqlc.arg(revoke_at)::timestamptz,
    updated_at = sqlc.arg(revoke_at)::timestamptz
WHERE ent.source_type = 'subscription'
  AND ent.source_id = $1
  AND ent.entitlement = $2
  AND ent.revoked_at IS NULL
  AND ent.deleted_at IS NULL;

-- name: SoftDeleteGraceBySubscription :exec
UPDATE billing.entitlements ent SET
    deleted_at = sqlc.arg(now)::timestamptz,
    updated_at = sqlc.arg(now)::timestamptz
WHERE ent.source_type = 'grace'
  AND ent.source_id = $1
  AND ent.revoked_at IS NULL
  AND ent.deleted_at IS NULL;

-- name: ListActiveGraceWindowsForUpdate :many
SELECT * FROM billing.entitlements ent
WHERE ent.source_type = 'grace'
  AND ent.source_id = $1
  AND ent.revoked_at IS NULL
  AND ent.deleted_at IS NULL
  AND ent.start_at < sqlc.arg(now)::timestamptz
  AND ent.end_at IS NOT NULL AND ent.end_at > sqlc.arg(now)::timestamptz
FOR UPDATE;

-- name: SoftDeleteFutureGraceBySubscription :exec
UPDATE billing.entitlements ent SET
    deleted_at = sqlc.arg(now)::timestamptz,
    updated_at = sqlc.arg(now)::timestamptz
WHERE ent.source_type = 'grace'
  AND ent.source_id = $1
  AND ent.revoked_at IS NULL
  AND ent.deleted_at IS NULL
  AND ent.start_at >= sqlc.arg(now)::timestamptz;

-- name: AcquireEntitlementTimelineLock :exec
-- Transaction-scoped advisory lock serializing timeline updates per
-- (subject, entitlement); the key is hashed in Go.
SELECT pg_advisory_xact_lock(sqlc.arg(key)::bigint);

-- name: ShiftEntitlementTimelineWindows :exec
UPDATE billing.entitlements ent SET
    start_at = ent.start_at + (sqlc.arg(delta_seconds)::bigint * interval '1 second'),
    end_at = CASE WHEN ent.end_at IS NULL THEN NULL
             ELSE ent.end_at + (sqlc.arg(delta_seconds)::bigint * interval '1 second') END,
    updated_at = sqlc.arg(now)::timestamptz
WHERE ent.tenant_subject_id = $1
  AND ent.entitlement = $2
  AND ent.revoked_at IS NULL
  AND ent.deleted_at IS NULL
  AND ent.start_at >= sqlc.arg(from_at)::timestamptz
  AND NOT (ent.id = ANY(sqlc.arg(exclude_ids)::uuid[]));

-- name: SetEntitlementEndAt :exec
UPDATE billing.entitlements ent SET
    end_at = sqlc.narg(end_at),
    updated_at = sqlc.arg(now)::timestamptz
WHERE ent.id = $1
  AND ent.revoked_at IS NULL
  AND ent.deleted_at IS NULL;

-- name: SoftDeleteLaterEntitlementWindows :exec
UPDATE billing.entitlements ent SET
    deleted_at = sqlc.arg(now)::timestamptz,
    updated_at = sqlc.arg(now)::timestamptz
WHERE ent.tenant_subject_id = $1
  AND ent.entitlement = $2
  AND ent.revoked_at IS NULL
  AND ent.deleted_at IS NULL
  AND ent.start_at >= sqlc.arg(from_at)::timestamptz
  AND ent.id <> sqlc.arg(exclude_id)::uuid;

-- name: RevokeEntitlementWindowNow :exec
UPDATE billing.entitlements ent SET
    end_at = sqlc.arg(end_at)::timestamptz,
    revoked_at = sqlc.arg(now)::timestamptz,
    revoke_reason = sqlc.narg(revoke_reason),
    updated_at = sqlc.arg(now)::timestamptz
WHERE ent.id = $1
  AND ent.revoked_at IS NULL
  AND ent.deleted_at IS NULL;
