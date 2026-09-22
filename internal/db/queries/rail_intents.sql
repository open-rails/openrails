-- #358 phase A: provider intent ledger queries — idempotent enqueue, the
-- executor's SKIP LOCKED lease claim, status transitions, supersede-by-subject
-- and relevance-window expiry. merchant_id is stamped explicitly by the
-- producers (request paths run on a merchant-pinned connection, so RLS
-- double-checks the stamp). The executor/verifier workers do NOT sweep
-- cross-merchant: there is no privileged pool, so they fan out over the
-- merchants a 0022 SECURITY DEFINER work queue names and run each pass inside
-- that merchant's own pinned scope (or#862).

-- =====================================================================
-- Enqueue (effectively-once per logical intent)
-- =====================================================================

-- Idempotent on (merchant_id, idempotency_key). Conflict semantics by current
-- status:
--   pending, attempts=0  -> refresh schedule/payload (no possible submission)
--   pending, attempts>0  -> preserve the original operation after reclaimed park
--   superseded | expired -> REVIVE: the intent became relevant again (e.g. a
--                           re-cancel after a resume superseded the delete);
--                           attempts/failure state reset
--   anything else        -> untouched (in_flight is owned by its lease;
--                           succeeded must never re-execute; failed_* keep
--                           their backoff/terminal state)
-- Frozen-payload tier changes (nmi_upgrade, stripe_tier_change) are never
-- refreshed: a same-key race must not replace the frozen commercial decision.
-- Always RETURNs the canonical row for the key.
-- name: EnqueueRailIntent :one
INSERT INTO openrails.rail_intents (
    merchant_id, rail, intent_type, subscription_id, payment_id, price_id,
    payload, idempotency_key, status, next_attempt_at, origin, origin_reason,
    actor, expires_at, psp_id, custodian_id
) VALUES (
    sqlc.arg(merchant_id), sqlc.arg(rail), sqlc.arg(intent_type),
    sqlc.narg(subscription_id), sqlc.narg(payment_id), sqlc.narg(price_id),
    sqlc.narg(payload), sqlc.arg(idempotency_key), 'pending',
    sqlc.arg(next_attempt_at)::timestamptz, sqlc.arg(origin),
    sqlc.narg(origin_reason), sqlc.narg(actor), sqlc.narg(expires_at),
    sqlc.narg(psp_id)::uuid, sqlc.narg(custodian_id)::uuid
)
ON CONFLICT (merchant_id, idempotency_key) DO UPDATE SET
    status = CASE
        WHEN openrails.rail_intents.intent_type NOT IN ('nmi_upgrade', 'stripe_tier_change', 'invoice_collection', 'manual_rebill', 'nmi_sale', 'initial_membership', 'subscription_collection') AND (openrails.rail_intents.status IN ('superseded', 'expired') OR (openrails.rail_intents.status = 'pending' AND openrails.rail_intents.attempts = 0)) THEN 'pending'
        ELSE openrails.rail_intents.status
    END,
    next_attempt_at = CASE
        WHEN openrails.rail_intents.intent_type NOT IN ('nmi_upgrade', 'stripe_tier_change', 'invoice_collection', 'manual_rebill', 'nmi_sale', 'initial_membership', 'subscription_collection') AND (openrails.rail_intents.status IN ('superseded', 'expired') OR (openrails.rail_intents.status = 'pending' AND openrails.rail_intents.attempts = 0)) THEN EXCLUDED.next_attempt_at
        ELSE openrails.rail_intents.next_attempt_at
    END,
    payload = CASE
        WHEN openrails.rail_intents.intent_type NOT IN ('nmi_upgrade', 'stripe_tier_change', 'invoice_collection', 'manual_rebill', 'nmi_sale', 'initial_membership', 'subscription_collection') AND (openrails.rail_intents.status IN ('superseded', 'expired') OR (openrails.rail_intents.status = 'pending' AND openrails.rail_intents.attempts = 0)) THEN EXCLUDED.payload
        ELSE openrails.rail_intents.payload
    END,
    psp_id = CASE
        WHEN openrails.rail_intents.intent_type NOT IN ('nmi_upgrade', 'stripe_tier_change', 'invoice_collection', 'manual_rebill', 'nmi_sale', 'initial_membership', 'subscription_collection') AND (openrails.rail_intents.status IN ('superseded', 'expired') OR (openrails.rail_intents.status = 'pending' AND openrails.rail_intents.attempts = 0)) THEN EXCLUDED.psp_id
        ELSE openrails.rail_intents.psp_id
    END,
    origin = CASE
        WHEN openrails.rail_intents.intent_type NOT IN ('nmi_upgrade', 'stripe_tier_change', 'invoice_collection', 'manual_rebill', 'nmi_sale', 'initial_membership', 'subscription_collection') AND (openrails.rail_intents.status IN ('superseded', 'expired') OR (openrails.rail_intents.status = 'pending' AND openrails.rail_intents.attempts = 0)) THEN EXCLUDED.origin
        ELSE openrails.rail_intents.origin
    END,
    origin_reason = CASE
        WHEN openrails.rail_intents.intent_type NOT IN ('nmi_upgrade', 'stripe_tier_change', 'invoice_collection', 'manual_rebill', 'nmi_sale', 'initial_membership', 'subscription_collection') AND (openrails.rail_intents.status IN ('superseded', 'expired') OR (openrails.rail_intents.status = 'pending' AND openrails.rail_intents.attempts = 0)) THEN EXCLUDED.origin_reason
        ELSE openrails.rail_intents.origin_reason
    END,
    actor = CASE
        WHEN openrails.rail_intents.intent_type NOT IN ('nmi_upgrade', 'stripe_tier_change', 'invoice_collection', 'manual_rebill', 'nmi_sale', 'initial_membership', 'subscription_collection') AND (openrails.rail_intents.status IN ('superseded', 'expired') OR (openrails.rail_intents.status = 'pending' AND openrails.rail_intents.attempts = 0)) THEN EXCLUDED.actor
        ELSE openrails.rail_intents.actor
    END,
    expires_at = CASE
        WHEN openrails.rail_intents.intent_type NOT IN ('nmi_upgrade', 'stripe_tier_change', 'invoice_collection', 'manual_rebill', 'nmi_sale', 'initial_membership', 'subscription_collection') AND (openrails.rail_intents.status IN ('superseded', 'expired') OR (openrails.rail_intents.status = 'pending' AND openrails.rail_intents.attempts = 0)) THEN EXCLUDED.expires_at
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

-- =====================================================================
-- Executor / verifier claims (single-executor lease, SKIP LOCKED)
-- =====================================================================

-- Claims due executable intents: pending/failed_retryable whose
-- next_attempt_at arrived, plus orphaned in_flight rows whose lease elapsed
-- (crashed executor; per-type semantics make the reclaim safe). Never claims
-- past the relevance window — those rows are swept by
-- ExpireOverdueRailIntents. Abandoned in-flight attempts are still claimed
-- after their deadline so possible submissions can be reconciled.
-- name: ClaimDueRailIntents :many
WITH due AS (
    SELECT id FROM openrails.rail_intents
    WHERE rail_intents.merchant_id = sqlc.arg(merchant_id)::uuid AND (
            (status IN ('pending', 'failed_retryable') AND next_attempt_at <= sqlc.arg(now)::timestamptz)
            OR (status = 'in_flight' AND claimed_until IS NOT NULL AND claimed_until <= sqlc.arg(now)::timestamptz)
          )
      AND (intent_type = 'subscription_collection' OR status = 'in_flight' OR (status = 'pending' AND attempts > 0) OR expires_at IS NULL OR expires_at > sqlc.arg(now)::timestamptz)
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
WHERE pi.merchant_id = sqlc.arg(merchant_id)::uuid AND pi.id = due.id
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
WHERE pi.merchant_id = sqlc.arg(merchant_id)::uuid AND pi.id = sqlc.arg(id)
  AND (
        pi.status IN ('pending', 'failed_retryable')
        OR (pi.status = 'in_flight' AND pi.claimed_until IS NOT NULL AND pi.claimed_until <= sqlc.arg(now)::timestamptz)
      )
  AND (pi.intent_type = 'subscription_collection' OR pi.status = 'in_flight' OR (pi.status = 'pending' AND pi.attempts > 0) OR pi.expires_at IS NULL OR pi.expires_at > sqlc.arg(now)::timestamptz)
RETURNING pi.*;

-- Claims due unknown_needs_verify intents for the verifier. Status stays
-- unknown_needs_verify (verification is a read, not an attempt — attempts is
-- not bumped); the lease alone prevents double-verification.
-- name: ClaimDueVerifyRailIntents :many
WITH due AS (
    SELECT id FROM openrails.rail_intents
    WHERE rail_intents.merchant_id = sqlc.arg(merchant_id)::uuid AND status = 'unknown_needs_verify'
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
WHERE pi.merchant_id = sqlc.arg(merchant_id)::uuid AND pi.id = due.id
RETURNING pi.*;

-- Claims ONE unknown operation for operator resolution. Like the verifier
-- claim, status and attempts are unchanged; the lease excludes a concurrent
-- verifier or resolver.
-- name: ClaimUnknownRailIntentByID :one
UPDATE openrails.rail_intents
SET claimed_until = sqlc.arg(lease_until)::timestamptz,
    updated_at = now()
WHERE rail_intents.merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)
  AND status = 'unknown_needs_verify'
  AND (claimed_until IS NULL OR claimed_until <= sqlc.arg(now)::timestamptz)
RETURNING *;

-- Releases a resolver lease after rejected evidence, leaving the operation
-- exactly as it was.
-- name: ReleaseUnknownRailIntentClaim :execrows
UPDATE openrails.rail_intents
SET claimed_until = NULL,
    updated_at = now()
WHERE rail_intents.merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id) AND status = 'unknown_needs_verify';

-- Renews a live claim while its handler runs (xs-007 row 32): the executor
-- beats this every lease/4, so claimed_until measures SILENCE from a dead
-- executor rather than how long a provider call may take. Renewal is refused
-- once the lease has lapsed — by then another executor may hold the row, and a
-- late beat must not steal it back. Returns rows affected (0 = lost).
-- name: RenewRailIntentClaim :execrows
UPDATE openrails.rail_intents
SET claimed_until = sqlc.arg(lease_until)::timestamptz,
    updated_at = now()
WHERE rail_intents.merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)
  AND status IN ('in_flight', 'unknown_needs_verify')
  AND claimed_until IS NOT NULL
  AND claimed_until > sqlc.arg(now)::timestamptz;

-- =====================================================================
-- Outcome transitions (always release the lease)
-- =====================================================================

-- name: MarkRailIntentSucceeded :execrows
UPDATE openrails.rail_intents
SET status = 'succeeded',
    executed_at = sqlc.arg(now)::timestamptz,
    result_evidence = CASE
        WHEN result_evidence ?| ARRAY['qualified_receipt', 'qualified_enrollment', 'account_requalifications']
        THEN COALESCE(sqlc.narg(result_evidence)::jsonb, '{}'::jsonb)
          || CASE WHEN result_evidence ? 'qualified_receipt'
                  THEN jsonb_build_object('qualified_receipt', result_evidence->'qualified_receipt') ELSE '{}'::jsonb END
          || CASE WHEN result_evidence ? 'qualified_enrollment'
                  THEN jsonb_build_object('qualified_enrollment', result_evidence->'qualified_enrollment') ELSE '{}'::jsonb END
          || CASE WHEN result_evidence ? 'account_requalifications'
                  THEN jsonb_build_object('account_requalifications', result_evidence->'account_requalifications') ELSE '{}'::jsonb END
        ELSE sqlc.narg(result_evidence)::jsonb
    END,
    last_failure_reason = NULL,
    claimed_until = NULL,
    updated_at = now()
WHERE rail_intents.merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id) AND status IN ('in_flight', 'unknown_needs_verify')
  AND intent_type NOT IN ('invoice_collection','manual_rebill','nmi_upgrade','stripe_tier_change','nmi_sale','initial_membership','nmi_vault_delete','hyperswitch_method_delete','subscription_collection');

-- name: MarkRailIntentFailedRetryable :execrows
UPDATE openrails.rail_intents
SET status = 'failed_retryable',
    next_attempt_at = sqlc.arg(next_attempt_at)::timestamptz,
    last_failure_reason = sqlc.arg(reason),
    claimed_until = NULL,
    updated_at = now()
WHERE rail_intents.merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id) AND status IN ('in_flight', 'unknown_needs_verify')
  AND NOT (intent_type IN ('invoice_collection','subscription_collection') AND rail <> 'stripe' AND coalesce(result_evidence, '{}'::jsonb) ? 'submitted_at')
  AND NOT (intent_type = 'nmi_sale' AND coalesce(result_evidence, '{}'::jsonb) ? 'sale_submitted')
  AND NOT (intent_type = 'initial_membership' AND coalesce(result_evidence, '{}'::jsonb) ? 'initial_submitted');

-- Ambiguous outcome (or a verify that stayed inconclusive): park for the
-- verifier, scheduled at next_attempt_at.
-- name: MarkRailIntentUnknown :execrows
UPDATE openrails.rail_intents
SET status = 'unknown_needs_verify',
    result_evidence = COALESCE(result_evidence, '{}'::jsonb) || COALESCE(sqlc.narg(result_evidence)::jsonb, '{}'::jsonb)
      || CASE WHEN result_evidence ? 'initial_submitted' THEN jsonb_build_object('initial_submitted',result_evidence->'initial_submitted') ELSE '{}'::jsonb END,
    next_attempt_at = sqlc.arg(next_attempt_at)::timestamptz,
    last_failure_reason = sqlc.arg(reason),
    claimed_until = NULL,
    updated_at = now()
WHERE rail_intents.merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id) AND status IN ('in_flight', 'unknown_needs_verify');

-- name: MarkRailIntentFailedTerminal :execrows
UPDATE openrails.rail_intents
SET status = 'failed_terminal',
    last_failure_reason = sqlc.arg(reason),
    result_evidence = CASE
        WHEN result_evidence ?| ARRAY['qualified_receipt', 'qualified_enrollment', 'account_requalifications']
        THEN COALESCE(sqlc.narg(result_evidence)::jsonb, '{}'::jsonb)
          || CASE WHEN result_evidence ? 'qualified_receipt'
                  THEN jsonb_build_object('qualified_receipt', result_evidence->'qualified_receipt') ELSE '{}'::jsonb END
          || CASE WHEN result_evidence ? 'qualified_enrollment'
                  THEN jsonb_build_object('qualified_enrollment', result_evidence->'qualified_enrollment') ELSE '{}'::jsonb END
          || CASE WHEN result_evidence ? 'account_requalifications'
                  THEN jsonb_build_object('account_requalifications', result_evidence->'account_requalifications') ELSE '{}'::jsonb END
        ELSE sqlc.narg(result_evidence)::jsonb
    END,
    claimed_until = NULL,
    updated_at = now()
WHERE rail_intents.merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id) AND status IN ('in_flight', 'unknown_needs_verify')
  AND intent_type NOT IN ('invoice_collection','manual_rebill','nmi_upgrade','stripe_tier_change','nmi_sale','initial_membership','nmi_vault_delete','hyperswitch_method_delete','subscription_collection');

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
WHERE rail_intents.merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id) AND status = 'in_flight'
  AND NOT (intent_type IN ('invoice_collection','subscription_collection') AND coalesce(result_evidence, '{}'::jsonb) ? 'submitted_at')
  -- A stale no-send result must not undo another executor's payment fence.
  AND (intent_type <> 'nmi_sale' OR NOT (COALESCE(result_evidence, '{}'::jsonb) ? 'sale_submitted'))
  AND (intent_type <> 'initial_membership' OR NOT (COALESCE(result_evidence, '{}'::jsonb) ? 'initial_submitted'));

-- name: MarkRailIntentSuperseded :execrows
UPDATE openrails.rail_intents
SET status = 'superseded',
    last_failure_reason = sqlc.arg(reason),
    claimed_until = NULL,
    updated_at = now()
WHERE rail_intents.merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id) AND status IN ('pending', 'in_flight', 'failed_retryable', 'unknown_needs_verify')
  AND NOT (intent_type='initial_membership' AND coalesce(result_evidence,'{}'::jsonb) ? 'initial_submitted')
  AND intent_type <> 'subscription_collection';

-- =====================================================================
-- Supersede-by-subject + relevance-window expiry
-- =====================================================================

-- Supersedes every live intent of one type for one subscription (e.g. a
-- resume superseding the pending deferred delete). in_flight rows are left to
-- their executor: its per-type relevance check re-verifies before acting, so
-- a racing supersede is advisory there.
-- name: SupersedeRailIntentsBySubject :execrows
UPDATE openrails.rail_intents
SET status = 'superseded',
    last_failure_reason = sqlc.arg(reason),
    updated_at = now()
WHERE rail_intents.merchant_id = sqlc.arg(merchant_id)::uuid AND intent_type = sqlc.arg(intent_type)
  AND subscription_id = sqlc.arg(subscription_id)
  AND (status = 'failed_retryable' OR (status = 'pending' AND attempts = 0))
  AND NOT (intent_type='initial_membership' AND coalesce(result_evidence,'{}'::jsonb) ? 'initial_submitted')
  AND intent_type <> 'subscription_collection';

-- #679: destructive intents held by the volume breaker (an OPEN
-- life.provider_intent.held_bulk finding for their merchant) never expire out
-- of the ledger while held — the operator's resolution decides their fate.
-- name: ExpireOverdueRailIntents :execrows
UPDATE openrails.rail_intents pi
SET status = 'expired',
    last_failure_reason = 'relevance window elapsed before execution',
    claimed_until = NULL,
    updated_at = now()
WHERE pi.merchant_id = sqlc.arg(merchant_id)::uuid AND (pi.status = 'failed_retryable' OR (pi.status = 'pending' AND pi.attempts = 0))
  AND NOT (pi.intent_type IN ('invoice_collection','subscription_collection') AND coalesce(pi.result_evidence, '{}'::jsonb) ? 'submitted_at')
  AND NOT (pi.intent_type = 'nmi_sale' AND coalesce(pi.result_evidence, '{}'::jsonb) ? 'sale_submitted')
  AND NOT (pi.intent_type='initial_membership' AND coalesce(pi.result_evidence,'{}'::jsonb) ? 'initial_submitted')
  AND pi.expires_at IS NOT NULL
  AND pi.expires_at <= sqlc.arg(now)::timestamptz
  AND NOT (
        pi.intent_type = ANY (sqlc.arg(breaker_held_types)::text[])
        AND EXISTS (
            SELECT 1 FROM openrails.reconciliation_findings f
            WHERE f.merchant_id = sqlc.arg(merchant_id)::uuid AND f.merchant_id = pi.merchant_id
              AND f.finding_type = 'life.provider_intent.held_bulk'
              AND f.status IN ('reconcile_required', 'requires_review')
        )
      )
  AND pi.intent_type <> 'subscription_collection';

-- =====================================================================
-- Reconcile (#107 PS-10): stuck-intent detection
-- =====================================================================

-- Non-terminal intents that have sat in the ledger beyond the reconcile
-- engine's hardcoded stuck thresholds: pending/failed_retryable older than the
-- action cutoff (24h), in_flight/unknown_needs_verify older than the verify
-- cutoff (2h — a healthy verifier resolves unknowns in minutes; an in_flight
-- lease outliving hours means a dead executor). Read-only; runs tenant-scoped
-- on the engine's tenant-pinned connection.
-- name: ListStuckRailIntents :many
SELECT * FROM openrails.rail_intents
WHERE rail_intents.merchant_id = sqlc.arg(merchant_id)::uuid AND ( (status IN ('pending', 'failed_retryable') AND created_at <= sqlc.arg(action_cutoff)::timestamptz)
   OR (status IN ('in_flight', 'unknown_needs_verify') AND created_at <= sqlc.arg(verify_cutoff)::timestamptz)
) ORDER BY created_at, id;

-- =====================================================================
-- Reads
-- =====================================================================

-- name: GetRailIntent :one
SELECT * FROM openrails.rail_intents WHERE rail_intents.merchant_id = sqlc.arg(merchant_id)::uuid AND id = $1;

-- name: CountRailIntents :one
SELECT count(*) FROM openrails.rail_intents
WHERE rail_intents.merchant_id = sqlc.arg(merchant_id)::uuid AND (sqlc.narg(status)::text IS NULL OR status = sqlc.narg(status)::text)
  AND (sqlc.narg(rail)::text IS NULL OR rail = sqlc.narg(rail)::text)
  AND (sqlc.narg(intent_type)::text IS NULL OR intent_type = sqlc.narg(intent_type)::text)
  AND (sqlc.narg(subscription_id)::uuid IS NULL OR subscription_id = sqlc.narg(subscription_id)::uuid);

-- name: ListRailIntents :many
SELECT * FROM openrails.rail_intents
WHERE rail_intents.merchant_id = sqlc.arg(merchant_id)::uuid AND (sqlc.narg(status)::text IS NULL OR status = sqlc.narg(status)::text)
  AND (sqlc.narg(rail)::text IS NULL OR rail = sqlc.narg(rail)::text)
  AND (sqlc.narg(intent_type)::text IS NULL OR intent_type = sqlc.narg(intent_type)::text)
  AND (sqlc.narg(subscription_id)::uuid IS NULL OR subscription_id = sqlc.narg(subscription_id)::uuid)
ORDER BY created_at DESC, id
LIMIT sqlc.arg(page_limit) OFFSET sqlc.arg(page_offset);

-- =====================================================================
-- #679 destructive-volume circuit breaker
-- =====================================================================

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
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND status = 'active'
  AND deleted_at IS NULL;

-- The breaker's per-merchant standing finding (stable identity —
-- merchant x finding_type x subject_key is UNIQUE).
-- name: GetReconciliationFindingByIdentity :one
SELECT * FROM openrails.reconciliation_findings
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND finding_type = sqlc.arg(finding_type)
  AND subject_key = sqlc.arg(subject_key);

-- =====================================================================
-- #732 anti-credential-compromise rate ceiling (per-actor + per-merchant)
-- =====================================================================
-- The durable rail_intents ledger IS the counter (#674): every destructive
-- user/admin op posts a row BEFORE it executes, so a rolling-hour COUNT over
-- created_at is the burst gauge. Counts by CREATION (created_at), not execution:
-- the ceiling stops the burst at the producer chokepoint, before the write-ahead
-- intent is even created. Both readers are migration 0021/0028 SECURITY DEFINER
-- functions — NOT the base pool. The base pool is not privileged: it is the same
-- openrails_app role, so a GUC-less count is not "everything", it is EMPTY
-- (or#824/or#860).

-- Destructive user/admin intents THIS actor created in the rolling window.
-- Deliberately CROSS-MERCHANT and unchanged by or#866: one stolen credential
-- operating across merchants is exactly the shape this leg must see, and an
-- actor is not a tenant, so there is no cross-tenant budget to share here.
-- name: CountDestructiveIntentsByActorSince :one
SELECT openrails.count_destructive_intents_by_actor_since(
    sqlc.arg(actor)::text,
    sqlc.arg(intent_types)::text[],
    sqlc.arg(since)::timestamptz);

-- ONE merchant's destructive intents in the rolling window, for a caller-supplied
-- origin set. Both walls of the ceiling are this same count (migration 0028):
--   * origins {user,admin} — the anti-theft wall. It used to be DEPLOYMENT-wide,
--     which made one merchant's ordinary customer cancellations refuse every
--     other merchant's (or#866, cross-tenant DoS). A forged-identity burst is
--     still walled at 15/h inside the merchant it targets.
--   * origins {system} — the automation wall (or#842): a runaway convergence or
--     poisoned roster inside ONE merchant, never a fleet-wide number that a
--     thousand merchants converging their own books would legitimately exceed.
-- System origin must never burn the anti-theft budget and vice versa: they are
-- separate windows over disjoint origin sets, counted separately.
-- name: CountDestructiveIntentsForMerchantSince :one
SELECT openrails.count_destructive_intents_for_merchant_since(
    sqlc.arg(merchant_id)::uuid,
    sqlc.arg(origins)::text[],
    sqlc.arg(intent_types)::text[],
    sqlc.arg(since)::timestamptz);

-- name: GetRailIntentByIdempotencyKey :one
SELECT * FROM openrails.rail_intents
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND idempotency_key = sqlc.arg(idempotency_key)::text;

-- The one unresolved tier change that owns a subscription
-- (uq_rail_intents_tier_change_subscription).
-- name: GetLiveTierChangeRailIntent :one
SELECT * FROM openrails.rail_intents
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND subscription_id = sqlc.arg(subscription_id)::uuid
  AND intent_type IN ('nmi_upgrade', 'stripe_tier_change')
  AND status IN ('pending', 'in_flight', 'unknown_needs_verify', 'failed_retryable');

-- name: LockRailIntentForCollectionCompletion :one
-- Call after acquiring the domain's payer/invoice locks, matching admission's
-- invoice-before-operation order.
SELECT * FROM openrails.rail_intents
WHERE id = sqlc.arg(id)::uuid AND merchant_id = sqlc.arg(merchant_id)::uuid
  AND intent_type IN ('invoice_collection', 'manual_rebill', 'subscription_collection')
FOR UPDATE;

-- name: CompleteRailIntentCollection :execrows
-- The caller owns the row lock and commits this transition with local effects.
UPDATE openrails.rail_intents
SET status = sqlc.arg(status)::text,
    result_evidence = sqlc.arg(evidence)::jsonb,
    last_failure_reason = NULLIF(sqlc.arg(reason)::text, ''),
    executed_at = CASE WHEN sqlc.arg(status)::text = 'succeeded' THEN sqlc.arg(now)::timestamptz ELSE executed_at END,
    claimed_until = NULL,
    updated_at = sqlc.arg(now)::timestamptz
WHERE id = sqlc.arg(id)::uuid AND merchant_id = sqlc.arg(merchant_id)::uuid
  AND intent_type IN ('invoice_collection', 'manual_rebill', 'subscription_collection')
  AND status IN ('in_flight', 'unknown_needs_verify');

-- name: RetainRailIntentQualifiedEvidence :execrows
-- Custody binds immutable provider facts to the accepted operation. A repeated
-- identical receipt succeeds; a conflicting receipt or terminal row never changes.
UPDATE openrails.rail_intents
SET result_evidence = COALESCE(result_evidence, '{}'::jsonb)
        || jsonb_build_object(sqlc.arg(evidence_key)::text, sqlc.arg(receipt)::jsonb),
    updated_at = now()
WHERE id = sqlc.arg(id)::uuid
  AND merchant_id = sqlc.arg(merchant_id)::uuid
  AND psp_id = sqlc.arg(psp_id)::uuid
  AND intent_type = sqlc.arg(intent_type)::text
  AND payload = sqlc.arg(payload)::jsonb
  AND ((sqlc.arg(evidence_key)::text = 'qualified_receipt' AND intent_type IN ('invoice_collection','manual_rebill','nmi_upgrade','nmi_sale','initial_membership', 'subscription_collection')
           AND NOT (COALESCE(result_evidence, '{}'::jsonb) ?| ARRAY['rebill_decline','stripe_recurring_decline','qualified_invoice_nonexecution','qualified_initial_refusal']))
       OR (sqlc.arg(evidence_key)::text = 'qualified_enrollment' AND intent_type IN ('nmi_upgrade','initial_membership')
           AND NOT (COALESCE(result_evidence, '{}'::jsonb) ? 'qualified_initial_refusal'))
       OR (sqlc.arg(evidence_key)::text='qualified_initial_refusal' AND intent_type='initial_membership'
           AND NOT (coalesce(result_evidence,'{}'::jsonb) ?| ARRAY['qualified_receipt','qualified_enrollment'])
           AND ((sqlc.arg(receipt)::jsonb->>'kind'='not_submitted' AND NOT (coalesce(result_evidence,'{}'::jsonb) ? 'initial_submitted'))
             OR (sqlc.arg(receipt)::jsonb->>'kind' IN ('provider_declined','stripe_canceled','not_dispatched') AND result_evidence->>'initial_submitted'='true')))
       OR (sqlc.arg(evidence_key)::text='stripe_recurring_decline' AND intent_type='subscription_collection' AND rail='stripe'
           AND COALESCE(result_evidence->>'submitted_at','') <> ''
           AND sqlc.arg(receipt)::jsonb->>'failure_code'='canceled'
           AND NOT (COALESCE(result_evidence,'{}'::jsonb) ?| ARRAY['qualified_receipt','rebill_decline','qualified_invoice_nonexecution']))
       OR (sqlc.arg(evidence_key)::text = 'qualified_invoice_nonexecution' AND intent_type IN ('invoice_collection','subscription_collection')
           AND COALESCE(result_evidence->>'submitted_at', '') = sqlc.arg(receipt)::jsonb->>'submitted_at'
           AND NOT (COALESCE(result_evidence, '{}'::jsonb) ? 'qualified_receipt')))
  AND status IN ('in_flight', 'unknown_needs_verify')
  AND (NOT (COALESCE(result_evidence, '{}'::jsonb) ? sqlc.arg(evidence_key)::text)
       OR result_evidence->sqlc.arg(evidence_key)::text = sqlc.arg(receipt)::jsonb);

-- name: RetainRailIntentCollectionCandidate :execrows
-- A possible provider reference is a candidate only; it never proves payment.
UPDATE openrails.rail_intents
SET result_evidence = COALESCE(result_evidence, '{}'::jsonb)
        || jsonb_build_object('collection_candidate', sqlc.arg(candidate)::jsonb),
    updated_at = now()
WHERE id = sqlc.arg(id)::uuid
  AND merchant_id = sqlc.arg(merchant_id)::uuid
  AND psp_id = sqlc.arg(psp_id)::uuid
  AND intent_type = sqlc.arg(intent_type)::text
  AND payload = sqlc.arg(payload)::jsonb
  AND status IN ('in_flight', 'unknown_needs_verify')
  AND (NOT (COALESCE(result_evidence, '{}'::jsonb) ? 'collection_candidate')
       OR result_evidence->'collection_candidate' = sqlc.arg(candidate)::jsonb);

-- name: GetUnresolvedManualRebill :one
-- Admission already owns the subscription lock. An unresolved charge retains
-- ownership even when another lifecycle observer has moved its period/status.
SELECT * FROM openrails.rail_intents
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND subscription_id = sqlc.arg(subscription_id)::uuid
  AND intent_type = 'manual_rebill'
  AND status IN ('pending', 'in_flight', 'unknown_needs_verify', 'failed_retryable')
ORDER BY created_at, id
LIMIT 1;

-- name: GetLatestManualRebillForPeriod :one
-- Accepted operation ordinals and counted financial declines are distinct: a
-- superseded pre-send attempt must not burn a dunning failure or reuse its key.
SELECT * FROM openrails.rail_intents
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND subscription_id = sqlc.arg(subscription_id)::uuid
  AND intent_type = 'manual_rebill'
  AND (payload->'renewal'->>'period_start')::timestamptz = sqlc.arg(period_start)::timestamptz
ORDER BY (payload->>'attempt')::integer DESC, id DESC
LIMIT 1;

-- name: RetainRailIntentRebillPreparation :execrows
UPDATE openrails.rail_intents
SET result_evidence = COALESCE(result_evidence, '{}'::jsonb) || jsonb_build_object('rebill_preparation', sqlc.arg(preparation)::jsonb),
    updated_at = now()
WHERE id = sqlc.arg(id)::uuid AND merchant_id = sqlc.arg(merchant_id)::uuid
  AND psp_id = sqlc.arg(psp_id)::uuid AND intent_type = 'manual_rebill'
  AND payload = sqlc.arg(payload)::jsonb
  AND status IN ('pending', 'in_flight', 'unknown_needs_verify', 'failed_retryable')
  AND (NOT (COALESCE(result_evidence, '{}'::jsonb) ? 'rebill_preparation')
       OR result_evidence->'rebill_preparation' = sqlc.arg(preparation)::jsonb);

-- name: RetainRailIntentRebillDecline :execrows
UPDATE openrails.rail_intents
SET result_evidence = COALESCE(result_evidence, '{}'::jsonb) || jsonb_build_object('rebill_decline', sqlc.arg(decline)::jsonb),
    updated_at = now()
WHERE id = sqlc.arg(id)::uuid AND merchant_id = sqlc.arg(merchant_id)::uuid
  AND psp_id = sqlc.arg(psp_id)::uuid AND intent_type IN ('manual_rebill','subscription_collection')
  AND payload = sqlc.arg(payload)::jsonb
  AND status IN ('in_flight', 'unknown_needs_verify')
  AND NOT (COALESCE(result_evidence, '{}'::jsonb) ? 'qualified_receipt')
  AND (NOT (COALESCE(result_evidence, '{}'::jsonb) ? 'rebill_decline')
       OR result_evidence->'rebill_decline' = sqlc.arg(decline)::jsonb);

-- name: ListCompletedManualRebillPaymentCoverage :many
-- Exact accepted operation provenance is decoded in Go. Do not infer an older
-- charge's billing interval from the time its local payment row was recovered.
SELECT sqlc.embed(i), p.id AS covered_payment_id, p.customer_id AS paid_customer_id,
       p.psp_id AS paid_psp_id, p.subscription_id AS paid_subscription_id,
       p.rail AS paid_rail, p.transaction_id AS paid_transaction_id, p.price_id AS paid_price_id,
       p.amount AS paid_amount, p.currency AS paid_currency
FROM openrails.payments p
JOIN openrails.rail_intents i ON i.merchant_id=p.merchant_id AND i.psp_id=p.psp_id
  AND i.subscription_id=p.subscription_id AND i.intent_type='manual_rebill'
  AND (i.result_evidence->>'transaction_id'=p.transaction_id
       OR i.result_evidence->'qualified_receipt'->'nmi'->>'transaction_id'=p.transaction_id)
WHERE p.merchant_id=sqlc.arg(merchant_id)::uuid
  AND p.subscription_id=sqlc.arg(subscription_id)::uuid
  AND p.psp_id=sqlc.arg(psp_id)::uuid
  AND p.status='completed' AND p.deleted_at IS NULL;

-- name: HasUnattributedPaymentAfterRebillBoundary :one
-- Existing provider-observed payments have no immutable accepted interval.
-- Retain their conservative timestamp refusal separately; it is not exact
-- coverage evidence. A matched operation is validated by the query above.
SELECT EXISTS (
 SELECT 1 FROM openrails.payments p
 WHERE p.merchant_id=sqlc.arg(merchant_id)::uuid
   AND p.subscription_id=sqlc.arg(subscription_id)::uuid
   AND p.psp_id=sqlc.arg(psp_id)::uuid
   AND p.status='completed' AND p.deleted_at IS NULL
   AND p.purchased_at>=sqlc.arg(period_start)::timestamptz
   AND NOT EXISTS (
     SELECT 1 FROM openrails.rail_intents i
     WHERE i.merchant_id=p.merchant_id AND i.psp_id=p.psp_id
       AND i.subscription_id=p.subscription_id AND i.intent_type='manual_rebill'
       AND (i.result_evidence->>'transaction_id'=p.transaction_id
            OR i.result_evidence->'qualified_receipt'->'nmi'->>'transaction_id'=p.transaction_id)
   )
)::bool;

-- Resume undoes only the current provider target's unsent cancellation. A
-- previous binding may retain an independent historical deletion obligation.
-- name: SupersedePendingNMIDelete :execrows
UPDATE openrails.rail_intents
SET status='superseded', last_failure_reason=sqlc.arg(reason), updated_at=now()
WHERE merchant_id=sqlc.arg(merchant_id)::uuid
  AND idempotency_key=sqlc.arg(idempotency_key)::text
  AND intent_type='nmi_delete_subscription'
  AND (status='failed_retryable' OR (status='pending' AND attempts=0));

-- A pending quote remains owned after a charge decline: provider preparation
-- may already have changed its recurring amount. Applied/canceled historical
-- quotes do not lock future price changes. Call under the subscription lock.
-- name: ListRebillTermOwners :many
SELECT i.*
FROM openrails.rail_intents i
JOIN openrails.subscriptions s ON s.id=i.subscription_id AND s.merchant_id=i.merchant_id
WHERE i.merchant_id=sqlc.arg(merchant_id)::uuid
  AND i.subscription_id=sqlc.arg(subscription_id)::uuid
  AND s.deleted_at IS NULL
  AND i.intent_type='manual_rebill'
  AND (
    i.status IN ('pending','in_flight','unknown_needs_verify','failed_retryable')
    OR EXISTS (
      SELECT 1 FROM openrails.subscription_reprices r
      WHERE r.merchant_id=i.merchant_id AND r.subscription_id=i.subscription_id
        AND r.merchant_id=sqlc.arg(merchant_id)::uuid
        AND r.status IN ('scheduled','blocked')
        AND r.id::text=i.payload->'renewal'->>'reprice_id'
    )
    OR s.scheduled_price_id::text=i.payload->'renewal'->>'scheduled_price_id'
  );

-- The subscription/domain lock precedes the operation lock, as at admission.
-- name: LockRailIntentForTierCompletion :one
SELECT * FROM openrails.rail_intents
WHERE id=sqlc.arg(id)::uuid AND merchant_id=sqlc.arg(merchant_id)::uuid
  AND intent_type IN ('nmi_upgrade','stripe_tier_change')
FOR UPDATE;

-- name: CompleteTierChangeOutcome :execrows
UPDATE openrails.rail_intents
SET status=sqlc.arg(status)::text,
    result_evidence=sqlc.arg(evidence)::jsonb,
    last_failure_reason=CASE WHEN sqlc.arg(status)::text='succeeded' THEN NULL ELSE sqlc.narg(reason)::text END,
    executed_at=CASE WHEN sqlc.arg(status)::text='succeeded' THEN sqlc.arg(now)::timestamptz ELSE executed_at END,
    claimed_until=NULL, updated_at=sqlc.arg(now)::timestamptz
WHERE id=sqlc.arg(id)::uuid AND merchant_id=sqlc.arg(merchant_id)::uuid
  AND intent_type IN ('nmi_upgrade','stripe_tier_change')
  AND status IN ('in_flight','unknown_needs_verify');

-- name: GetUnresolvedSaleForCustomerProduct :one
SELECT * FROM openrails.rail_intents
WHERE merchant_id=sqlc.arg(merchant_id)::uuid AND intent_type='nmi_sale'
  AND payload->>'user_id'=sqlc.arg(customer_id)::text
  AND payload->>'product_id'=sqlc.arg(product_id)::text
  AND status IN ('pending','in_flight','unknown_needs_verify','failed_retryable')
ORDER BY created_at LIMIT 1;

-- name: LockRailIntentForSaleCompletion :one
SELECT * FROM openrails.rail_intents
WHERE id=sqlc.arg(id)::uuid AND merchant_id=sqlc.arg(merchant_id)::uuid AND intent_type='nmi_sale'
FOR UPDATE;

-- name: CompleteSaleOutcome :execrows
UPDATE openrails.rail_intents
SET status=sqlc.arg(status)::text, result_evidence=sqlc.arg(evidence)::jsonb,
    last_failure_reason=sqlc.narg(reason)::text, executed_at=sqlc.arg(now)::timestamptz,
    claimed_until=NULL, updated_at=sqlc.arg(now)::timestamptz
WHERE id=sqlc.arg(id)::uuid AND merchant_id=sqlc.arg(merchant_id)::uuid
  AND intent_type='nmi_sale' AND status IN ('in_flight','unknown_needs_verify');

-- name: ListRetainedSalesForArchive :many
SELECT * FROM openrails.rail_intents
WHERE merchant_id=sqlc.arg(merchant_id)::uuid AND intent_type='nmi_sale'
  AND (sqlc.narg(after_id)::uuid IS NULL OR id>sqlc.narg(after_id)::uuid)
ORDER BY id LIMIT sqlc.arg(page_size)::int;

-- name: GetUnresolvedInitialEnrollmentForCustomerProduct :one
SELECT * FROM openrails.rail_intents
WHERE merchant_id=sqlc.arg(merchant_id)::uuid AND intent_type='initial_membership'
  AND payload->'terms'->>'customer_id'=sqlc.arg(customer_id)::text
  AND payload->'terms'->>'product_id'=sqlc.arg(product_id)::text
  AND status IN ('pending','in_flight','unknown_needs_verify','failed_retryable')
ORDER BY created_at LIMIT 1;

-- name: LockRailIntentForInitialEnrollmentCompletion :one
SELECT * FROM openrails.rail_intents
WHERE id=sqlc.arg(id)::uuid AND merchant_id=sqlc.arg(merchant_id)::uuid AND intent_type='initial_membership'
FOR UPDATE;

-- name: CompleteInitialEnrollmentOutcome :execrows
UPDATE openrails.rail_intents
SET status=sqlc.arg(status)::text, result_evidence=sqlc.arg(evidence)::jsonb,
    last_failure_reason=sqlc.narg(reason)::text, executed_at=sqlc.arg(now)::timestamptz,
    claimed_until=NULL, updated_at=sqlc.arg(now)::timestamptz
WHERE id=sqlc.arg(id)::uuid AND merchant_id=sqlc.arg(merchant_id)::uuid
  AND intent_type='initial_membership' AND status IN ('in_flight','unknown_needs_verify');
-- name: GetUnresolvedSubscriptionCollection :one
SELECT * FROM openrails.rail_intents
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND subscription_id = sqlc.arg(subscription_id)::uuid
  AND intent_type = 'subscription_collection'
  AND status IN ('pending', 'in_flight', 'unknown_needs_verify', 'failed_retryable')
ORDER BY created_at, id LIMIT 1;

-- name: GetLatestSubscriptionCollectionForPeriod :one
SELECT * FROM openrails.rail_intents
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND subscription_id = sqlc.arg(subscription_id)::uuid
  AND intent_type = 'subscription_collection'
  AND (payload->>'previous_period_end')::timestamptz = sqlc.arg(previous_period_end)::timestamptz
ORDER BY (payload->>'attempt')::integer DESC, id DESC LIMIT 1;

-- name: ListRetainedInitialEnrollmentsForArchive :many
SELECT * FROM openrails.rail_intents
WHERE merchant_id=sqlc.arg(merchant_id)::uuid AND intent_type='initial_membership'
  AND (sqlc.narg(after_id)::uuid IS NULL OR id>sqlc.narg(after_id)::uuid)
ORDER BY id LIMIT sqlc.arg(page_size)::int;

-- name: ListInitialEnrollmentsForMembership :many
SELECT * FROM openrails.rail_intents
WHERE merchant_id=sqlc.arg(merchant_id)::uuid AND intent_type='initial_membership'
  AND payload->'terms'->>'subscription_id'=sqlc.arg(subscription_id)::uuid::text
ORDER BY id LIMIT 2;

-- name: ListRetainedSubscriptionCollectionsForArchive :many
SELECT * FROM openrails.rail_intents
WHERE merchant_id=sqlc.arg(merchant_id)::uuid AND intent_type='subscription_collection'
  AND (sqlc.narg(after_id)::uuid IS NULL OR id>sqlc.narg(after_id)::uuid)
ORDER BY id LIMIT sqlc.arg(page_size)::int;

-- name: ExpireRailIntentByID :execrows
UPDATE openrails.rail_intents pi
SET status = 'expired',
    last_failure_reason = 'relevance window elapsed before execution',
    claimed_until = NULL,
    updated_at = now()
WHERE pi.id = sqlc.arg(id)::uuid AND pi.merchant_id = sqlc.arg(merchant_id)::uuid AND (pi.status = 'failed_retryable' OR (pi.status = 'pending' AND pi.attempts = 0))
  AND NOT (pi.intent_type IN ('invoice_collection','subscription_collection') AND coalesce(pi.result_evidence, '{}'::jsonb) ? 'submitted_at')
  AND NOT (pi.intent_type = 'nmi_sale' AND coalesce(pi.result_evidence, '{}'::jsonb) ? 'sale_submitted')
  AND NOT (pi.intent_type='initial_membership' AND coalesce(pi.result_evidence,'{}'::jsonb) ? 'initial_submitted')
  AND pi.expires_at IS NOT NULL
  AND pi.expires_at <= sqlc.arg(now)::timestamptz
  AND NOT (
        pi.intent_type = ANY (sqlc.arg(breaker_held_types)::text[])
        AND EXISTS (
            SELECT 1 FROM openrails.reconciliation_findings f
            WHERE f.merchant_id = sqlc.arg(merchant_id)::uuid AND f.merchant_id = pi.merchant_id
              AND f.finding_type = 'life.provider_intent.held_bulk'
              AND f.status IN ('reconcile_required', 'requires_review')
        )
      )
  AND pi.intent_type <> 'subscription_collection';


-- name: RecoverAbandonedRailIntentByID :execrows
UPDATE openrails.rail_intents
SET status = 'unknown_needs_verify', claimed_until = NULL, next_attempt_at = sqlc.arg(now)::timestamptz,
    last_failure_reason = 'executor lease expired; verify before retry', updated_at = now()
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid
  AND status = 'in_flight' AND (claimed_until IS NULL OR claimed_until <= sqlc.arg(now)::timestamptz);
