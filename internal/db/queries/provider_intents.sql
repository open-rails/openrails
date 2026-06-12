-- #358 phase A: provider intent ledger queries — idempotent enqueue, the
-- executor's SKIP LOCKED lease claim, status transitions, supersede-by-subject
-- and relevance-window expiry. tenant_id is stamped explicitly by the
-- producers (request paths run on a tenant-pinned connection, so RLS
-- double-checks the stamp); the executor/verifier workers sweep cross-tenant
-- on the privileged pool like the other River workers.

-- ============================================================================
-- Enqueue (effectively-once per logical intent)
-- ============================================================================

-- Idempotent on (tenant_id, idempotency_key). Conflict semantics by current
-- status:
--   pending              -> refresh schedule/payload (latest enqueue wins)
--   superseded | expired -> REVIVE: the intent became relevant again (e.g. a
--                           re-cancel after a resume superseded the delete);
--                           attempts/failure state reset
--   anything else        -> untouched (in_flight is owned by its lease;
--                           succeeded must never re-execute; failed_* keep
--                           their backoff/terminal state)
-- Always RETURNs the canonical row for the key.
-- name: EnqueueProviderIntent :one
INSERT INTO billing.provider_intents (
    tenant_id, provider, intent_type, subscription_id, payment_id, price_id,
    payload, idempotency_key, status, next_attempt_at, origin, origin_reason,
    expires_at, account_fingerprint
) VALUES (
    sqlc.arg(tenant_id), sqlc.arg(provider), sqlc.arg(intent_type),
    sqlc.narg(subscription_id), sqlc.narg(payment_id), sqlc.narg(price_id),
    sqlc.narg(payload), sqlc.arg(idempotency_key), 'pending',
    sqlc.arg(next_attempt_at)::timestamptz, sqlc.arg(origin),
    sqlc.narg(origin_reason), sqlc.narg(expires_at),
    sqlc.narg(account_fingerprint)
)
ON CONFLICT (tenant_id, idempotency_key) DO UPDATE SET
    status = CASE
        WHEN billing.provider_intents.status IN ('pending', 'superseded', 'expired') THEN 'pending'
        ELSE billing.provider_intents.status
    END,
    next_attempt_at = CASE
        WHEN billing.provider_intents.status IN ('pending', 'superseded', 'expired') THEN EXCLUDED.next_attempt_at
        ELSE billing.provider_intents.next_attempt_at
    END,
    payload = CASE
        WHEN billing.provider_intents.status IN ('pending', 'superseded', 'expired') THEN EXCLUDED.payload
        ELSE billing.provider_intents.payload
    END,
    account_fingerprint = CASE
        WHEN billing.provider_intents.status IN ('pending', 'superseded', 'expired') THEN EXCLUDED.account_fingerprint
        ELSE billing.provider_intents.account_fingerprint
    END,
    origin = CASE
        WHEN billing.provider_intents.status IN ('pending', 'superseded', 'expired') THEN EXCLUDED.origin
        ELSE billing.provider_intents.origin
    END,
    origin_reason = CASE
        WHEN billing.provider_intents.status IN ('pending', 'superseded', 'expired') THEN EXCLUDED.origin_reason
        ELSE billing.provider_intents.origin_reason
    END,
    expires_at = CASE
        WHEN billing.provider_intents.status IN ('pending', 'superseded', 'expired') THEN EXCLUDED.expires_at
        ELSE billing.provider_intents.expires_at
    END,
    attempts = CASE
        WHEN billing.provider_intents.status IN ('superseded', 'expired') THEN 0
        ELSE billing.provider_intents.attempts
    END,
    last_failure_reason = CASE
        WHEN billing.provider_intents.status IN ('superseded', 'expired') THEN NULL
        ELSE billing.provider_intents.last_failure_reason
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
-- ExpireOverdueProviderIntents.
-- name: ClaimDueProviderIntents :many
WITH due AS (
    SELECT id FROM billing.provider_intents
    WHERE (
            (status IN ('pending', 'failed_retryable') AND next_attempt_at <= sqlc.arg(now)::timestamptz)
            OR (status = 'in_flight' AND claimed_until IS NOT NULL AND claimed_until <= sqlc.arg(now)::timestamptz)
          )
      AND (expires_at IS NULL OR expires_at > sqlc.arg(now)::timestamptz)
    ORDER BY next_attempt_at
    LIMIT sqlc.arg(batch_size)
    FOR UPDATE SKIP LOCKED
)
UPDATE billing.provider_intents pi
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
-- name: ClaimProviderIntentByID :one
UPDATE billing.provider_intents pi
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
-- name: ClaimDueVerifyProviderIntents :many
WITH due AS (
    SELECT id FROM billing.provider_intents
    WHERE status = 'unknown_needs_verify'
      AND next_attempt_at <= sqlc.arg(now)::timestamptz
      AND (claimed_until IS NULL OR claimed_until <= sqlc.arg(now)::timestamptz)
    ORDER BY next_attempt_at
    LIMIT sqlc.arg(batch_size)
    FOR UPDATE SKIP LOCKED
)
UPDATE billing.provider_intents pi
SET claimed_until = sqlc.arg(lease_until)::timestamptz,
    updated_at = now()
FROM due
WHERE pi.id = due.id
RETURNING pi.*;

-- ============================================================================
-- Outcome transitions (always release the lease)
-- ============================================================================

-- name: MarkProviderIntentSucceeded :execrows
UPDATE billing.provider_intents
SET status = 'succeeded',
    executed_at = sqlc.arg(now)::timestamptz,
    result_evidence = sqlc.narg(result_evidence),
    last_failure_reason = NULL,
    claimed_until = NULL,
    updated_at = now()
WHERE id = sqlc.arg(id) AND status IN ('in_flight', 'unknown_needs_verify');

-- name: MarkProviderIntentFailedRetryable :execrows
UPDATE billing.provider_intents
SET status = 'failed_retryable',
    next_attempt_at = sqlc.arg(next_attempt_at)::timestamptz,
    last_failure_reason = sqlc.arg(reason),
    claimed_until = NULL,
    updated_at = now()
WHERE id = sqlc.arg(id) AND status IN ('in_flight', 'unknown_needs_verify');

-- Ambiguous outcome (or a verify that stayed inconclusive): park for the
-- verifier, scheduled at next_attempt_at.
-- name: MarkProviderIntentUnknown :execrows
UPDATE billing.provider_intents
SET status = 'unknown_needs_verify',
    next_attempt_at = sqlc.arg(next_attempt_at)::timestamptz,
    last_failure_reason = sqlc.arg(reason),
    claimed_until = NULL,
    updated_at = now()
WHERE id = sqlc.arg(id) AND status IN ('in_flight', 'unknown_needs_verify');

-- name: MarkProviderIntentFailedTerminal :execrows
UPDATE billing.provider_intents
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
-- name: ParkProviderIntent :execrows
UPDATE billing.provider_intents
SET status = 'pending',
    attempts = GREATEST(attempts - 1, 0),
    next_attempt_at = sqlc.arg(next_attempt_at)::timestamptz,
    last_failure_reason = sqlc.arg(reason),
    claimed_until = NULL,
    updated_at = now()
WHERE id = sqlc.arg(id) AND status = 'in_flight';

-- name: MarkProviderIntentSuperseded :execrows
UPDATE billing.provider_intents
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
-- name: SupersedeProviderIntentsBySubject :execrows
UPDATE billing.provider_intents
SET status = 'superseded',
    last_failure_reason = sqlc.arg(reason),
    updated_at = now()
WHERE intent_type = sqlc.arg(intent_type)
  AND subscription_id = sqlc.arg(subscription_id)
  AND status IN ('pending', 'failed_retryable', 'unknown_needs_verify');

-- name: ExpireOverdueProviderIntents :execrows
UPDATE billing.provider_intents
SET status = 'expired',
    last_failure_reason = 'relevance window elapsed before execution',
    claimed_until = NULL,
    updated_at = now()
WHERE status IN ('pending', 'failed_retryable', 'unknown_needs_verify')
  AND expires_at IS NOT NULL
  AND expires_at <= sqlc.arg(now)::timestamptz;

-- ============================================================================
-- Reconcile (#107 PS-10): stuck-intent detection
-- ============================================================================

-- Non-terminal intents that have sat in the ledger beyond the reconcile
-- engine's hardcoded stuck thresholds: pending/failed_retryable older than the
-- action cutoff (24h), in_flight/unknown_needs_verify older than the verify
-- cutoff (2h — a healthy verifier resolves unknowns in minutes; an in_flight
-- lease outliving hours means a dead executor). Read-only; runs tenant-scoped
-- on the engine's tenant-pinned connection.
-- name: ListStuckProviderIntents :many
SELECT * FROM billing.provider_intents
WHERE (status IN ('pending', 'failed_retryable') AND created_at <= sqlc.arg(action_cutoff)::timestamptz)
   OR (status IN ('in_flight', 'unknown_needs_verify') AND created_at <= sqlc.arg(verify_cutoff)::timestamptz)
ORDER BY created_at, id;

-- ============================================================================
-- Reads
-- ============================================================================

-- name: GetProviderIntent :one
SELECT * FROM billing.provider_intents WHERE id = $1;

-- name: GetProviderIntentByIdempotencyKey :one
SELECT * FROM billing.provider_intents
WHERE tenant_id = sqlc.arg(tenant_id) AND idempotency_key = sqlc.arg(idempotency_key);

-- name: CountProviderIntents :one
SELECT count(*) FROM billing.provider_intents
WHERE (sqlc.narg(status)::text IS NULL OR status = sqlc.narg(status)::text)
  AND (sqlc.narg(provider)::text IS NULL OR provider = sqlc.narg(provider)::text)
  AND (sqlc.narg(intent_type)::text IS NULL OR intent_type = sqlc.narg(intent_type)::text)
  AND (sqlc.narg(subscription_id)::uuid IS NULL OR subscription_id = sqlc.narg(subscription_id)::uuid);

-- name: ListProviderIntents :many
SELECT * FROM billing.provider_intents
WHERE (sqlc.narg(status)::text IS NULL OR status = sqlc.narg(status)::text)
  AND (sqlc.narg(provider)::text IS NULL OR provider = sqlc.narg(provider)::text)
  AND (sqlc.narg(intent_type)::text IS NULL OR intent_type = sqlc.narg(intent_type)::text)
  AND (sqlc.narg(subscription_id)::uuid IS NULL OR subscription_id = sqlc.narg(subscription_id)::uuid)
ORDER BY created_at DESC, id
LIMIT sqlc.arg(page_limit) OFFSET sqlc.arg(page_offset);

-- #365 escape hatch: after the operator confirms a credential change points at
-- the SAME (or intentionally-adopted) provider account, re-stamp the LIVE
-- intents to the current fingerprint so the account guard stops parking them.
-- Only live statuses — succeeded/terminal/superseded/expired rows keep the
-- fingerprint they executed (or died) under as evidence.
-- name: RefingerprintProviderIntents :execrows
UPDATE billing.provider_intents
SET account_fingerprint = sqlc.arg(account_fingerprint),
    updated_at = now()
WHERE provider = sqlc.arg(provider)
  AND status IN ('pending', 'failed_retryable', 'unknown_needs_verify')
  AND account_fingerprint IS DISTINCT FROM sqlc.arg(account_fingerprint);
