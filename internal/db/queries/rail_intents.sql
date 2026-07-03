-- #358 phase A: provider intent ledger queries — idempotent enqueue, the
-- executor's SKIP LOCKED lease claim, status transitions, supersede-by-subject
-- and relevance-window expiry. merchant_id is stamped explicitly by the
-- producers (request paths run on a tenant-pinned connection, so RLS
-- double-checks the stamp); the executor/verifier workers sweep cross-tenant
-- on the privileged pool like the other River workers.

-- ============================================================================
-- Enqueue (effectively-once per logical intent)
-- ============================================================================

-- Idempotent on (merchant_id, idempotency_key). Conflict semantics by current
-- status:
--   pending              -> refresh schedule/payload (latest enqueue wins)
--   superseded | expired -> REVIVE: the intent became relevant again (e.g. a
--                           re-cancel after a resume superseded the delete);
--                           attempts/failure state reset
--   anything else        -> untouched (in_flight is owned by its lease;
--                           succeeded must never re-execute; failed_* keep
--                           their backoff/terminal state)
-- Always RETURNs the canonical row for the key.
-- name: EnqueueRailIntent :one
INSERT INTO openrails.rail_intents (
    merchant_id, rail, intent_type, subscription_id, payment_id, price_id,
    payload, idempotency_key, status, next_attempt_at, origin, origin_reason,
    actor, expires_at, rail_merchant_account_id
) VALUES (
    sqlc.arg(merchant_id), sqlc.arg(rail), sqlc.arg(intent_type),
    sqlc.narg(subscription_id), sqlc.narg(payment_id), sqlc.narg(price_id),
    sqlc.narg(payload), sqlc.arg(idempotency_key), 'pending',
    sqlc.arg(next_attempt_at)::timestamptz, sqlc.arg(origin),
    sqlc.narg(origin_reason), sqlc.narg(actor), sqlc.narg(expires_at),
    sqlc.narg(rail_merchant_account_id)
)
ON CONFLICT (merchant_id, idempotency_key) DO UPDATE SET
    status = CASE
        WHEN openrails.rail_intents.status IN ('pending', 'superseded', 'expired') THEN 'pending'
        ELSE openrails.rail_intents.status
    END,
    next_attempt_at = CASE
        WHEN openrails.rail_intents.status IN ('pending', 'superseded', 'expired') THEN EXCLUDED.next_attempt_at
        ELSE openrails.rail_intents.next_attempt_at
    END,
    payload = CASE
        WHEN openrails.rail_intents.status IN ('pending', 'superseded', 'expired') THEN EXCLUDED.payload
        ELSE openrails.rail_intents.payload
    END,
    rail_merchant_account_id = CASE
        WHEN openrails.rail_intents.status IN ('pending', 'superseded', 'expired') THEN EXCLUDED.rail_merchant_account_id
        ELSE openrails.rail_intents.rail_merchant_account_id
    END,
    origin = CASE
        WHEN openrails.rail_intents.status IN ('pending', 'superseded', 'expired') THEN EXCLUDED.origin
        ELSE openrails.rail_intents.origin
    END,
    origin_reason = CASE
        WHEN openrails.rail_intents.status IN ('pending', 'superseded', 'expired') THEN EXCLUDED.origin_reason
        ELSE openrails.rail_intents.origin_reason
    END,
    actor = CASE
        WHEN openrails.rail_intents.status IN ('pending', 'superseded', 'expired') THEN EXCLUDED.actor
        ELSE openrails.rail_intents.actor
    END,
    expires_at = CASE
        WHEN openrails.rail_intents.status IN ('pending', 'superseded', 'expired') THEN EXCLUDED.expires_at
        ELSE openrails.rail_intents.expires_at
    END,
    attempts = CASE
        WHEN openrails.rail_intents.status IN ('superseded', 'expired') THEN 0
        ELSE openrails.rail_intents.attempts
    END,
    last_failure_reason = CASE
        WHEN openrails.rail_intents.status IN ('superseded', 'expired') THEN NULL
        ELSE openrails.rail_intents.last_failure_reason
    END,
    updated_at = now()
RETURNING *;

-- ============================================================================
-- Executor / verifier claims (single-executor lease, SKIP LOCKED)
-- ============================================================================

-- Claims due executable intents: pending/failed_retryable whose
-- next_attempt_at arrived, plus orphaned in_flight rows whose lease elapsed
-- (crashed executor; per-type semantics make the reclaim safe). Never claims
-- past the relevance window — those rows are swept by
-- ExpireOverdueRailIntents.
-- name: ClaimDueRailIntents :many
WITH due AS (
    SELECT id FROM openrails.rail_intents
    WHERE (
            (status IN ('pending', 'failed_retryable') AND next_attempt_at <= sqlc.arg(now)::timestamptz)
            OR (status = 'in_flight' AND claimed_until IS NOT NULL AND claimed_until <= sqlc.arg(now)::timestamptz)
          )
      AND (expires_at IS NULL OR expires_at > sqlc.arg(now)::timestamptz)
    ORDER BY next_attempt_at
    LIMIT sqlc.arg(batch_size)
    FOR UPDATE SKIP LOCKED
)
UPDATE openrails.rail_intents pi
SET status = 'in_flight',
    claimed_until = sqlc.arg(lease_until)::timestamptz,
    attempts = pi.attempts + 1,
    updated_at = now()
FROM due
WHERE pi.id = due.id
RETURNING pi.*;

-- Claims ONE specific intent for the synchronous execute path (#358 phase B):
-- a producer that just enqueued an intent leases it immediately and runs it
-- through the same execute/classify pipeline. Deliberately ignores
-- next_attempt_at — the interactive caller asked for the attempt NOW — but
-- honors the relevance window and existing leases; anything not claimable here
-- is drained by the scheduled executor instead.
-- name: ClaimRailIntentByID :one
UPDATE openrails.rail_intents pi
SET status = 'in_flight',
    claimed_until = sqlc.arg(lease_until)::timestamptz,
    attempts = pi.attempts + 1,
    updated_at = now()
WHERE pi.id = sqlc.arg(id)
  AND (
        pi.status IN ('pending', 'failed_retryable')
        OR (pi.status = 'in_flight' AND pi.claimed_until IS NOT NULL AND pi.claimed_until <= sqlc.arg(now)::timestamptz)
      )
  AND (pi.expires_at IS NULL OR pi.expires_at > sqlc.arg(now)::timestamptz)
RETURNING pi.*;

-- Claims due unknown_needs_verify intents for the verifier. Status stays
-- unknown_needs_verify (verification is a read, not an attempt — attempts is
-- not bumped); the lease alone prevents double-verification.
-- name: ClaimDueVerifyRailIntents :many
WITH due AS (
    SELECT id FROM openrails.rail_intents
    WHERE status = 'unknown_needs_verify'
      AND next_attempt_at <= sqlc.arg(now)::timestamptz
      AND (claimed_until IS NULL OR claimed_until <= sqlc.arg(now)::timestamptz)
    ORDER BY next_attempt_at
    LIMIT sqlc.arg(batch_size)
    FOR UPDATE SKIP LOCKED
)
UPDATE openrails.rail_intents pi
SET claimed_until = sqlc.arg(lease_until)::timestamptz,
    updated_at = now()
FROM due
WHERE pi.id = due.id
RETURNING pi.*;

-- ============================================================================
-- Outcome transitions (always release the lease)
-- ============================================================================

-- name: MarkRailIntentSucceeded :execrows
UPDATE openrails.rail_intents
SET status = 'succeeded',
    executed_at = sqlc.arg(now)::timestamptz,
    result_evidence = sqlc.narg(result_evidence),
    last_failure_reason = NULL,
    claimed_until = NULL,
    updated_at = now()
WHERE id = sqlc.arg(id) AND status IN ('in_flight', 'unknown_needs_verify');

-- name: MarkRailIntentFailedRetryable :execrows
UPDATE openrails.rail_intents
SET status = 'failed_retryable',
    next_attempt_at = sqlc.arg(next_attempt_at)::timestamptz,
    last_failure_reason = sqlc.arg(reason),
    claimed_until = NULL,
    updated_at = now()
WHERE id = sqlc.arg(id) AND status IN ('in_flight', 'unknown_needs_verify');

-- Ambiguous outcome (or a verify that stayed inconclusive): park for the
-- verifier, scheduled at next_attempt_at.
-- name: MarkRailIntentUnknown :execrows
UPDATE openrails.rail_intents
SET status = 'unknown_needs_verify',
    next_attempt_at = sqlc.arg(next_attempt_at)::timestamptz,
    last_failure_reason = sqlc.arg(reason),
    claimed_until = NULL,
    updated_at = now()
WHERE id = sqlc.arg(id) AND status IN ('in_flight', 'unknown_needs_verify');

-- name: MarkRailIntentFailedTerminal :execrows
UPDATE openrails.rail_intents
SET status = 'failed_terminal',
    last_failure_reason = sqlc.arg(reason),
    result_evidence = sqlc.narg(result_evidence),
    claimed_until = NULL,
    updated_at = now()
WHERE id = sqlc.arg(id) AND status IN ('in_flight', 'unknown_needs_verify');

-- Park: the attempt was deliberately NOT made (mode gate, kill switch,
-- unconfigured client). The intent goes back to pending with the reason
-- recorded and the claim's attempts bump undone — a park is not a failure and
-- must not escalate backoff.
-- name: ParkRailIntent :execrows
UPDATE openrails.rail_intents
SET status = 'pending',
    attempts = GREATEST(attempts - 1, 0),
    next_attempt_at = sqlc.arg(next_attempt_at)::timestamptz,
    last_failure_reason = sqlc.arg(reason),
    claimed_until = NULL,
    updated_at = now()
WHERE id = sqlc.arg(id) AND status = 'in_flight';

-- name: MarkRailIntentSuperseded :execrows
UPDATE openrails.rail_intents
SET status = 'superseded',
    last_failure_reason = sqlc.arg(reason),
    claimed_until = NULL,
    updated_at = now()
WHERE id = sqlc.arg(id) AND status IN ('pending', 'in_flight', 'failed_retryable', 'unknown_needs_verify');

-- ============================================================================
-- Supersede-by-subject + relevance-window expiry
-- ============================================================================

-- Supersedes every live intent of one type for one subscription (e.g. a
-- resume superseding the pending deferred delete). in_flight rows are left to
-- their executor: its per-type relevance check re-verifies before acting, so
-- a racing supersede is advisory there.
-- name: SupersedeRailIntentsBySubject :execrows
UPDATE openrails.rail_intents
SET status = 'superseded',
    last_failure_reason = sqlc.arg(reason),
    updated_at = now()
WHERE intent_type = sqlc.arg(intent_type)
  AND subscription_id = sqlc.arg(subscription_id)
  AND status IN ('pending', 'failed_retryable', 'unknown_needs_verify');

-- #679: destructive intents held by the volume breaker (an OPEN
-- life.provider_intent.held_bulk finding for their merchant) never expire out
-- of the ledger while held — the operator's resolution decides their fate.
-- name: ExpireOverdueRailIntents :execrows
UPDATE openrails.rail_intents pi
SET status = 'expired',
    last_failure_reason = 'relevance window elapsed before execution',
    claimed_until = NULL,
    updated_at = now()
WHERE pi.status IN ('pending', 'failed_retryable', 'unknown_needs_verify')
  AND pi.expires_at IS NOT NULL
  AND pi.expires_at <= sqlc.arg(now)::timestamptz
  AND NOT (
        pi.intent_type = ANY (sqlc.arg(breaker_held_types)::text[])
        AND EXISTS (
            SELECT 1 FROM openrails.reconciliation_findings f
            WHERE f.merchant_id = pi.merchant_id
              AND f.finding_type = 'life.provider_intent.held_bulk'
              AND f.status IN ('reconcile_required', 'requires_review')
        )
      );

-- ============================================================================
-- Reconcile (#107 PS-10): stuck-intent detection
-- ============================================================================

-- Non-terminal intents that have sat in the ledger beyond the reconcile
-- engine's hardcoded stuck thresholds: pending/failed_retryable older than the
-- action cutoff (24h), in_flight/unknown_needs_verify older than the verify
-- cutoff (2h — a healthy verifier resolves unknowns in minutes; an in_flight
-- lease outliving hours means a dead executor). Read-only; runs tenant-scoped
-- on the engine's tenant-pinned connection.
-- name: ListStuckRailIntents :many
SELECT * FROM openrails.rail_intents
WHERE (status IN ('pending', 'failed_retryable') AND created_at <= sqlc.arg(action_cutoff)::timestamptz)
   OR (status IN ('in_flight', 'unknown_needs_verify') AND created_at <= sqlc.arg(verify_cutoff)::timestamptz)
ORDER BY created_at, id;

-- ============================================================================
-- Reads
-- ============================================================================

-- name: GetRailIntent :one
SELECT * FROM openrails.rail_intents WHERE id = $1;

-- name: CountRailIntents :one
SELECT count(*) FROM openrails.rail_intents
WHERE (sqlc.narg(status)::text IS NULL OR status = sqlc.narg(status)::text)
  AND (sqlc.narg(rail)::text IS NULL OR rail = sqlc.narg(rail)::text)
  AND (sqlc.narg(intent_type)::text IS NULL OR intent_type = sqlc.narg(intent_type)::text)
  AND (sqlc.narg(subscription_id)::uuid IS NULL OR subscription_id = sqlc.narg(subscription_id)::uuid);

-- name: ListRailIntents :many
SELECT * FROM openrails.rail_intents
WHERE (sqlc.narg(status)::text IS NULL OR status = sqlc.narg(status)::text)
  AND (sqlc.narg(rail)::text IS NULL OR rail = sqlc.narg(rail)::text)
  AND (sqlc.narg(intent_type)::text IS NULL OR intent_type = sqlc.narg(intent_type)::text)
  AND (sqlc.narg(subscription_id)::uuid IS NULL OR subscription_id = sqlc.narg(subscription_id)::uuid)
ORDER BY created_at DESC, id
LIMIT sqlc.arg(page_limit) OFFSET sqlc.arg(page_offset);

-- ============================================================================
-- #679 destructive-volume circuit breaker
-- ============================================================================

-- Destructive intents that REACHED the provider in the rolling window:
-- succeeded rows count by executed_at; unresolved attempt outcomes
-- (unknown_needs_verify / failed_*) count by their last transition. in_flight
-- rows are deliberately EXCLUDED — the batch claim marks whole batches
-- in_flight before anything executes, and parks (pending) were never attempted.
-- name: CountDestructiveRailIntentsExecutedSince :one
SELECT count(*) FROM openrails.rail_intents
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND intent_type = ANY (sqlc.arg(intent_types)::text[])
  AND (
        (executed_at IS NOT NULL AND executed_at >= sqlc.arg(since)::timestamptz)
        OR (status IN ('unknown_needs_verify', 'failed_retryable', 'failed_terminal')
            AND updated_at >= sqlc.arg(since)::timestamptz)
      );

-- The breaker's budget baseline: max(floor, pct of active subscriptions).
-- name: CountActiveSubscriptionsByMerchant :one
SELECT count(*) FROM openrails.subscriptions
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND status = 'active';

-- The breaker's per-merchant standing finding (stable identity —
-- merchant x finding_type x subject_key is UNIQUE).
-- name: GetReconciliationFindingByIdentity :one
SELECT * FROM openrails.reconciliation_findings
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND finding_type = sqlc.arg(finding_type)
  AND subject_key = sqlc.arg(subject_key);

-- ============================================================================
-- #732 anti-credential-compromise rate ceiling (per-actor + global)
-- ============================================================================
-- The durable rail_intents ledger IS the counter (#674): every destructive
-- user/admin op posts a row BEFORE it executes, so a rolling-hour COUNT over
-- created_at is the burst gauge. Scoped to origin IN ('user','admin') — the
-- credential-theft surface; origin='system' (automated dunning / decline
-- cleanup) is #679's job and must NOT burn the anti-theft budget. Counts by
-- CREATION (created_at), not execution: the ceiling stops the burst at the
-- producer chokepoint, before the write-ahead intent is even created. These
-- run cross-merchant (per-actor AND global), so callers must execute them on a
-- non-merchant-pinned connection (the base pool), like the other cross-tenant
-- worker sweeps.

-- Destructive user/admin intents THIS actor created in the rolling window.
-- name: CountDestructiveIntentsByActorSince :one
SELECT count(*) FROM openrails.rail_intents
WHERE actor = sqlc.arg(actor)::text
  AND origin IN ('user', 'admin')
  AND intent_type = ANY (sqlc.arg(intent_types)::text[])
  AND created_at >= sqlc.arg(since)::timestamptz;

-- Destructive user/admin intents ALL actors + ALL merchants created in the
-- rolling window — the absolute frying-protection ceiling even if many actor
-- identities are forged.
-- name: CountDestructiveIntentsGlobalSince :one
SELECT count(*) FROM openrails.rail_intents
WHERE origin IN ('user', 'admin')
  AND intent_type = ANY (sqlc.arg(intent_types)::text[])
  AND created_at >= sqlc.arg(since)::timestamptz;
