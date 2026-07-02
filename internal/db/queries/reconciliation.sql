-- #107 phase 2: reconciliation runs + findings persistence, the engine's
-- merchant-scoped local-state reads, and the enforce appliers' idempotent local
-- writes. merchant_id is stamped explicitly (multi-merchant writer pattern); all
-- statements run on a merchant-pinned connection so RLS double-checks the stamp.

-- ============================================================================
-- Run lifecycle
-- ============================================================================

-- name: CreateReconciliationRun :one
INSERT INTO openrails.reconciliation_runs (
    merchant_id, mode, providers, window_since, window_until, started_at, status
) VALUES (
    sqlc.arg(merchant_id), sqlc.arg(mode), sqlc.arg(providers),
    sqlc.narg(window_since), sqlc.narg(window_until), now(), 'running'
)
RETURNING *;

-- name: FinishReconciliationRun :execrows
UPDATE openrails.reconciliation_runs
SET status = sqlc.arg(status),
    summary = sqlc.narg(summary),
    error = sqlc.narg(error),
    finished_at = now()
WHERE id = sqlc.arg(id) AND status = 'running';

-- name: GetReconciliationRun :one
SELECT * FROM openrails.reconciliation_runs WHERE id = $1;

-- name: GetLatestReconciliationRun :one
SELECT * FROM openrails.reconciliation_runs
ORDER BY started_at DESC
LIMIT 1;

-- name: ListReconciliationRuns :many
SELECT * FROM openrails.reconciliation_runs
ORDER BY started_at DESC
LIMIT sqlc.arg(page_limit) OFFSET sqlc.arg(page_offset);

-- ============================================================================
-- Findings: stable-identity upsert + lifecycle
-- ============================================================================

-- Re-runs UPDATE the standing finding for (merchant, finding_type, subject_key).
-- A previously fixed/auto_fixed finding that reappears is
-- REOPENED with the freshly computed status; an ignored finding stays ignored
-- (an operator explicitly silenced this identity).
-- name: UpsertReconciliationFinding :one
INSERT INTO openrails.reconciliation_findings (
    merchant_id, finding_type, subject_key, severity, status,
    recommended_action, evidence, resolved_at, resolution,
    first_seen_run, last_seen_run
) VALUES (
    sqlc.arg(merchant_id), sqlc.arg(finding_type), sqlc.arg(subject_key),
    sqlc.arg(severity), sqlc.arg(status), sqlc.narg(recommended_action),
    sqlc.narg(evidence)::jsonb,
    CASE WHEN sqlc.arg(status)::text = 'auto_fixed' THEN now() ELSE NULL END,
    CASE WHEN sqlc.arg(status)::text = 'auto_fixed' THEN 'enforced' ELSE NULL END,
    sqlc.arg(run_id), sqlc.arg(run_id)
)
ON CONFLICT (merchant_id, finding_type, subject_key) DO UPDATE SET
    severity = EXCLUDED.severity,
    status = CASE
        WHEN openrails.reconciliation_findings.status = 'ignored' THEN 'ignored'
        ELSE EXCLUDED.status
    END,
    recommended_action = EXCLUDED.recommended_action,
    evidence = CASE
        WHEN openrails.reconciliation_findings.status = 'ignored' THEN openrails.reconciliation_findings.evidence
        ELSE EXCLUDED.evidence
    END,
    resolved_at = CASE
        WHEN openrails.reconciliation_findings.status = 'ignored' THEN openrails.reconciliation_findings.resolved_at
        WHEN EXCLUDED.status = 'auto_fixed' THEN EXCLUDED.resolved_at
        ELSE NULL
    END,
    resolution = CASE
        WHEN openrails.reconciliation_findings.status = 'ignored' THEN openrails.reconciliation_findings.resolution
        WHEN EXCLUDED.status = 'auto_fixed' THEN EXCLUDED.resolution
        ELSE NULL
    END,
    last_seen_run = EXCLUDED.last_seen_run,
    last_seen_at = now(),
    updated_at = now()
RETURNING *;

-- name: GetReconciliationFinding :one
SELECT * FROM openrails.reconciliation_findings WHERE id = $1;

-- name: ListReconciliationFindings :many
SELECT * FROM openrails.reconciliation_findings
WHERE (sqlc.narg(status)::text IS NULL OR status = sqlc.narg(status)::text)
  AND (sqlc.narg(provider)::text IS NULL OR evidence->>'provider' = sqlc.narg(provider)::text)
  AND (sqlc.narg(finding_type)::text IS NULL OR finding_type = sqlc.narg(finding_type)::text)
  AND (NOT sqlc.arg(only_review_queue)::boolean OR status = 'requires_review')
ORDER BY last_seen_at DESC, id
LIMIT sqlc.arg(page_limit) OFFSET sqlc.arg(page_offset);

-- name: ListActionableReconciliationFindingsByProvider :many
SELECT * FROM openrails.reconciliation_findings
WHERE evidence->>'provider' = $1 AND status IN ('reconcile_required', 'requires_review')
ORDER BY finding_type, subject_key;

-- Findings of the given state-roster types absent from the just-completed run
-- covering their provider "vanished on their own" (design decision 1).
-- name: AutoResolveVanishedReconciliationFindings :execrows
UPDATE openrails.reconciliation_findings
SET status = 'fixed',
    resolution = 'auto_vanished',
    resolved_at = now(),
    updated_at = now()
WHERE evidence->>'provider' = sqlc.arg(provider)
  AND status IN ('reconcile_required', 'requires_review')
  AND last_seen_run <> sqlc.arg(run_id)
  AND finding_type = ANY (sqlc.arg(finding_types)::text[]);

-- life.provider_intent.stuck findings recover subject-first: an open finding
-- whose intent no longer meets the stuck criteria (executed, superseded, or
-- re-scheduled) auto-resolves on the next LIFE pass. Cutoffs mirror the
-- detection (ListStuckRailIntents) exactly — edit together.
-- name: AutoResolveRecoveredStuckIntentFindings :execrows
UPDATE openrails.reconciliation_findings f
SET status = 'fixed',
    resolution = 'auto_vanished',
    resolved_at = now(),
    updated_at = now()
WHERE f.merchant_id = sqlc.arg(merchant_id)::uuid
  AND f.finding_type = 'life.provider_intent.stuck'
  AND f.status IN ('reconcile_required', 'requires_review')
  AND NOT EXISTS (
      SELECT 1 FROM openrails.rail_intents pi
      WHERE pi.merchant_id = f.merchant_id
        AND pi.id::text = f.subject_key
        AND ((pi.status IN ('pending', 'failed_retryable') AND pi.created_at <= sqlc.arg(action_cutoff)::timestamptz)
          OR (pi.status IN ('in_flight', 'unknown_needs_verify') AND pi.created_at <= sqlc.arg(verify_cutoff)::timestamptz))
  );

-- name: MarkReconciliationFindingVanished :execrows
UPDATE openrails.reconciliation_findings
SET status = 'fixed',
    resolution = 'auto_vanished',
    resolved_at = now(),
    updated_at = now()
WHERE id = sqlc.arg(id) AND status IN ('reconcile_required', 'requires_review');

-- name: MarkReconciliationFindingAutoFixed :execrows
UPDATE openrails.reconciliation_findings
SET status = 'auto_fixed',
    resolution = 'enforced',
    evidence = jsonb_set(COALESCE(evidence, '{}'::jsonb), '{resolution}', sqlc.narg(resolution_evidence)::jsonb, true),
    resolved_at = now(),
    updated_at = now()
WHERE id = sqlc.arg(id) AND status IN ('reconcile_required', 'requires_review');

-- name: AckReconciliationFinding :execrows
UPDATE openrails.reconciliation_findings
SET status = 'fixed',
    resolution = 'admin_fixed',
    operator_notes = sqlc.narg(operator_notes),
    resolved_at = now(),
    updated_at = now()
WHERE id = sqlc.arg(id) AND status IN ('reconcile_required', 'requires_review', 'auto_fixed');

-- name: DismissReconciliationFinding :execrows
UPDATE openrails.reconciliation_findings
SET status = 'ignored',
    resolution = 'ignored',
    operator_notes = sqlc.narg(operator_notes),
    resolved_at = now(),
    updated_at = now()
WHERE id = sqlc.arg(id) AND status IN ('reconcile_required', 'requires_review', 'auto_fixed', 'fixed');

-- ============================================================================
-- #692 operator findings queue (admin API)
-- ============================================================================

-- The operator work list. Default view = OPEN findings only; an explicit
-- status filter overrides it (e.g. status=ignored). Sort: severity desc
-- (critical first) then age desc (oldest first). total_count rides every row
-- for pagination. merchant_id stamped explicitly (multi-merchant pattern);
-- RLS double-checks on merchant-pinned connections.
-- name: AdminListReconciliationFindings :many
SELECT sqlc.embed(f), count(*) OVER () AS total_count
FROM openrails.reconciliation_findings f
WHERE f.merchant_id = sqlc.arg(merchant_id)::uuid
  AND (CASE
         WHEN sqlc.narg(status)::text IS NULL THEN f.status IN ('reconcile_required', 'requires_review')
         ELSE f.status = sqlc.narg(status)::text
       END)
  AND (sqlc.narg(severity)::text IS NULL OR f.severity = sqlc.narg(severity)::text)
  AND (sqlc.narg(finding_type)::text IS NULL OR f.finding_type = sqlc.narg(finding_type)::text)
ORDER BY CASE f.severity WHEN 'critical' THEN 0 WHEN 'high' THEN 1 WHEN 'medium' THEN 2 ELSE 3 END,
         f.created_at, f.id
LIMIT sqlc.arg(page_limit) OFFSET sqlc.arg(page_offset);

-- Gauge input (#690): open-finding counts per (type, severity). The Go layer
-- folds these into the named gauges (freeloaders, duplicate_coverage,
-- open-by-severity) — the findings ledger IS the metric store.
-- name: CountOpenReconciliationFindingsByTypeSeverity :many
SELECT finding_type, severity, count(*) AS open_count
FROM openrails.reconciliation_findings
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND status IN ('reconcile_required', 'requires_review')
GROUP BY finding_type, severity;

-- Approve: the recommendation executed; mark fixed/admin_fixed with operator
-- notes + attribution, and record the execution evidence under
-- evidence.resolution. Only OPEN findings resolve — approve on an already-
-- resolved finding is a handler-level 409.
-- name: AdminResolveReconciliationFinding :execrows
UPDATE openrails.reconciliation_findings
SET status = 'fixed',
    resolution = 'admin_fixed',
    operator_notes = sqlc.narg(operator_notes),
    resolved_by = sqlc.arg(resolved_by),
    evidence = CASE
        WHEN sqlc.narg(resolution_evidence)::jsonb IS NULL THEN evidence
        ELSE jsonb_set(COALESCE(evidence, '{}'::jsonb), '{resolution}', sqlc.narg(resolution_evidence)::jsonb, true)
    END,
    resolved_at = now(),
    updated_at = now()
WHERE id = sqlc.arg(id) AND status IN ('reconcile_required', 'requires_review');

-- Ignore: permanent silence for the subject (the upsert keeps ignored
-- identities ignored across re-runs — same semantics the breaker's dismiss
-- honors). Notes are REQUIRED (enforced at the handler).
-- name: AdminIgnoreReconciliationFinding :execrows
UPDATE openrails.reconciliation_findings
SET status = 'ignored',
    resolution = 'ignored',
    operator_notes = sqlc.narg(operator_notes),
    resolved_by = sqlc.arg(resolved_by),
    resolved_at = now(),
    updated_at = now()
WHERE id = sqlc.arg(id) AND status IN ('reconcile_required', 'requires_review');

-- Partial failure: append the execution error to operator_notes; the finding
-- STAYS OPEN (never half-marked fixed).
-- name: AppendReconciliationFindingNotes :execrows
UPDATE openrails.reconciliation_findings
SET operator_notes = CASE
        WHEN COALESCE(operator_notes, '') = '' THEN sqlc.arg(note)::text
        ELSE operator_notes || E'\n' || sqlc.arg(note)::text
    END,
    updated_at = now()
WHERE id = sqlc.arg(id) AND status IN ('reconcile_required', 'requires_review');

-- ============================================================================
-- Local-state reads for the diff engine
-- ============================================================================

-- name: ReconcileListSubscriptionsByRails :many
SELECT id, customer_id, price_id, product_id, status, rail,
       rail_subscription_id, user_email, payment_method_id,
       current_period_starts_at, current_period_ends_at, started_at, ended_at,
       cancelled_at, cancel_type, deletion_scheduled_at, tier_group,
       last_retry_at, retry_attempts, next_retry_at,
       entitlements_spec_snapshot
FROM openrails.subscriptions
WHERE rail = ANY (sqlc.arg(rails)::text[])
  AND (sqlc.narg(rail_merchant_account_id)::uuid IS NULL OR rail_merchant_account_id = sqlc.narg(rail_merchant_account_id)::uuid);

-- name: ReconcileListPaymentsByTransactionIDs :many
SELECT id, customer_id, rail, transaction_id, amount, status,
       subscription_id, refunded_payment_id, purchased_at
FROM openrails.payments
WHERE rail::text = ANY (sqlc.arg(rails)::text[])
  AND transaction_id = ANY (sqlc.arg(transaction_ids)::text[])
  AND (sqlc.narg(rail_merchant_account_id)::uuid IS NULL OR rail_merchant_account_id = sqlc.narg(rail_merchant_account_id)::uuid);

-- name: ReconcileListPaymentMethodsByRails :many
-- Reconcile is NMI-vault-specific: rail_customer_ref IS the NMI customer_vault_id
-- here, aliased to vault_id so the reconcile matcher keeps its NMI-vault vocabulary.
SELECT id, customer_id, rail, rail_customer_ref AS vault_id, last_four, card_type,
       expiry_date
FROM openrails.payment_methods
WHERE rail = ANY (sqlc.arg(rails)::text[])
  AND (sqlc.narg(rail_merchant_account_id)::uuid IS NULL OR rail_merchant_account_id = sqlc.narg(rail_merchant_account_id)::uuid);

-- name: ReconcileListSolanaSubscriptionRefs :many
SELECT subscription_pda, plan_pda, subscriber_wallet
FROM openrails.solana_subscriptions;

-- Billable prices with their rail link blobs (provider_links): the PS-1
-- materializer maps a remote plan id onto the local price whose rails
-- jsonb carries that id under the provider's key. Draft prices are excluded
-- (not billable); archived prices stay (grandfathered subscriptions bill them).
-- name: ReconcileListPricesWithRails :many
SELECT id, product_id, amount, currency, access_duration_hours, auto_renew, status, rails
FROM openrails.prices
WHERE rails IS NOT NULL
  AND status <> 'draft';

-- ============================================================================
-- Enforce appliers: idempotent LOCAL writes only (never a provider call)
-- ============================================================================

-- #665: the PS-2 cancel / PS-3 adopt SQL appliers are gone — subscription
-- state transitions route through the ONE decider (reconcile.Decide) applied
-- via the shared lifecycle chokepoints (reconcile.ApplyDecision).

-- DERIVE-plane derive.grant_effect.mismatch revoke
-- repair: revoke the LIVE subscription-sourced entitlements of one
-- subscription. Admin grants and grace windows are different source types and
-- are untouchable by construction.
-- PS-4/PS-1: grant one subscription-sourced entitlement window unless an
-- equivalent live window already exists (idempotent via NOT EXISTS; the
-- re-run inserts nothing).
-- name: ReconcileGrantSubscriptionEntitlement :execrows
INSERT INTO openrails.entitlements (
    merchant_id, customer_id, entitlement, start_at, end_at,
    source_id, source_type
)
SELECT sqlc.arg(merchant_id), sqlc.arg(customer_id), sqlc.arg(entitlement),
       sqlc.arg(start_at)::timestamptz, sqlc.narg(end_at)::timestamptz,
       sqlc.arg(subscription_id), 'subscription'
WHERE NOT EXISTS (
    SELECT 1 FROM openrails.entitlements ent
    WHERE ent.customer_id = sqlc.arg(customer_id)
      AND ent.entitlement = sqlc.arg(entitlement)
      AND ent.source_type = 'subscription'
      AND ent.source_id = sqlc.arg(subscription_id)
      AND ent.revoked_at IS NULL
      AND ent.deleted_at IS NULL
      AND (ent.end_at IS NULL OR ent.end_at > sqlc.arg(now)::timestamptz)
);

-- PS-4: backfill a rail charge that has no local payment record.
-- Dedupe rides the uq_payments_merchant_rail_transaction identity.
-- name: ReconcileBackfillPayment :execrows
INSERT INTO openrails.payments (
    merchant_id, price_id, rail, transaction_id, amount, list_amount, currency,
    status, subscription_id, metadata, purchased_at, customer_id, rail_merchant_account_id
) VALUES (
    sqlc.arg(merchant_id)::uuid,
    sqlc.arg(price_id), sqlc.arg(rail)::text,
    sqlc.arg(transaction_id), sqlc.arg(amount), sqlc.arg(amount),
    sqlc.arg(currency),
    'completed', sqlc.narg(subscription_id), sqlc.narg(metadata),
    COALESCE(NULLIF(sqlc.arg(purchased_at)::timestamptz, '0001-01-01 00:00:00+00'::timestamptz), now()),
    sqlc.arg(customer_id), sqlc.narg(rail_merchant_account_id)
)
ON CONFLICT DO NOTHING;

-- PS-5: record a rail refund that is missing locally as a negative-
-- amount payment row linked to the refunded payment. Same dedupe identity.
-- name: ReconcileRecordRefund :execrows
INSERT INTO openrails.payments (
    merchant_id, price_id, rail, transaction_id, amount, list_amount, currency,
    status, subscription_id, refunded_payment_id, metadata, purchased_at,
    customer_id, rail_merchant_account_id
) VALUES (
    sqlc.arg(merchant_id)::uuid,
    sqlc.arg(price_id), sqlc.arg(rail)::text,
    sqlc.arg(transaction_id), sqlc.arg(amount), sqlc.arg(amount),
    sqlc.arg(currency),
    'completed', sqlc.narg(subscription_id), sqlc.narg(refunded_payment_id),
    sqlc.narg(metadata),
    COALESCE(NULLIF(sqlc.arg(purchased_at)::timestamptz, '0001-01-01 00:00:00+00'::timestamptz), now()),
    sqlc.arg(customer_id), sqlc.narg(rail_merchant_account_id)
)
ON CONFLICT DO NOTHING;

-- name: ReconcileMarkPaymentRefunded :execrows
UPDATE openrails.payments
SET status = 'refunded'
WHERE id = sqlc.arg(id) AND status <> 'refunded';

-- PS-1 materialization (bootstrap mode, --materialize): create the local
-- subscription for a rail subscription that resolved unambiguously to an
-- identity and a price. The entitlements/credits specs snapshot from the
-- product exactly like a normal signup, so the subscription-sourced
-- entitlement path works unchanged. Idempotent: a second run inserts nothing
-- when any subscription already carries the rail subscription id (zero
-- rows returned = already materialized).
-- name: ReconcileMaterializeSubscription :many
INSERT INTO openrails.subscriptions (
    merchant_id, price_id, product_id, status, rail, rail_subscription_id,
    user_email, current_period_starts_at, current_period_ends_at, started_at,
    entitlements_spec_snapshot, credits_spec_snapshot, customer_id, rail_merchant_account_id
)
SELECT sqlc.arg(merchant_id)::uuid, pr.id, pr.product_id, sqlc.arg(status)::openrails.subscription_status,
       sqlc.arg(rail), sqlc.arg(rail_subscription_id),
       sqlc.narg(user_email),
       sqlc.narg(period_starts_at)::timestamptz,
       sqlc.narg(period_ends_at)::timestamptz,
       COALESCE(sqlc.narg(started_at)::timestamptz, now()),
       p.entitlements_spec, p.credits_spec, sqlc.arg(customer_id), sqlc.narg(rail_merchant_account_id)
FROM openrails.prices pr
JOIN openrails.products p ON p.id = pr.product_id
WHERE pr.id = sqlc.arg(price_id)
  AND NOT EXISTS (
      SELECT 1 FROM openrails.subscriptions s
      WHERE s.rail_subscription_id = sqlc.arg(rail_subscription_id)
        AND s.rail = ANY (sqlc.arg(rails)::text[])
        AND (sqlc.narg(rail_merchant_account_id)::uuid IS NULL OR s.rail_merchant_account_id = sqlc.narg(rail_merchant_account_id)::uuid)
  )
RETURNING id, entitlements_spec_snapshot;

-- PS-7: adopt the rail's vault metadata for a stored payment method.
-- name: ReconcileAdoptPaymentMethod :execrows
UPDATE openrails.payment_methods
SET last_four = COALESCE(NULLIF(sqlc.arg(last_four)::text, ''), last_four),
    expiry_date = COALESCE(NULLIF(sqlc.arg(expiry_date)::text, ''), expiry_date),
    updated_at = now()
WHERE id = sqlc.arg(id)
  AND (last_four IS DISTINCT FROM NULLIF(sqlc.arg(last_four)::text, '')
       OR expiry_date IS DISTINCT FROM NULLIF(sqlc.arg(expiry_date)::text, ''));

-- #511 Convergence Engine: per-(merchant, source_domain) confirmed-absence gate.
-- WRITERS (#665): reconcile.MarkReconciledSourceDomains flips a domain
-- automatically after a pull PROVES it — exhaustive coverage
-- (SnapshotCoverage, not mere event-window watermark freshness) of EVERY
-- configured provider account whose rail could hold that domain's sources.
-- `grants` is admin/local-sourced, so no pull ever proves it — it stays a
-- manual/bulk-import decision. The flag is a ratchet: never auto-unset.

-- name: UpsertReconciliationState :one
-- Mark a source domain's reconciliation watermark. Pass fully_reconciled=true +
-- last_full_pull_at after a completed authoritative pull/import for that domain.
INSERT INTO openrails.reconciliation_state (
    merchant_id, source_domain, fully_reconciled, last_full_pull_at, updated_at
) VALUES (
    sqlc.arg(merchant_id)::uuid, sqlc.arg(source_domain)::text,
    sqlc.arg(fully_reconciled)::boolean, sqlc.narg(last_full_pull_at)::timestamptz, now()
)
ON CONFLICT (merchant_id, source_domain) DO UPDATE SET
    fully_reconciled = EXCLUDED.fully_reconciled,
    last_full_pull_at = COALESCE(EXCLUDED.last_full_pull_at, openrails.reconciliation_state.last_full_pull_at),
    updated_at = now()
RETURNING *;

-- name: IsSourceDomainReconciled :one
-- The confirmed-absence gate (§3.2): is this source domain proven fully
-- reconciled for the merchant? Absent row = not yet reconciled = false.
SELECT COALESCE((
    SELECT fully_reconciled FROM openrails.reconciliation_state
    WHERE merchant_id = sqlc.arg(merchant_id)::uuid
      AND source_domain = sqlc.arg(source_domain)::text
), false) AS fully_reconciled;

-- #665: the ONE lapsed-cohort scan. Selects candidate rows — active past the
-- period end, or past_due with grace elapsed and no retry scheduled — together
-- with their #664 evidence legs; the decider (reconcile.Decide) chooses the
-- transition in Go. No WHERE-clause complements to keep in sync: cohort
-- exclusivity is structural (one query, one total decision function).
-- Evidence legs:
--   payment_opened_period    — a completed payment opened the current period
--                              (ownership: OpenRails billed/observed it)
--   renewal_payment_after_end — a completed payment at/after the period end
--                              (billing DID happen; the advance path owns it)
--   watermark_newer_than_period_end — provider truth synced since the lapse
--                              and saw no renewal (ownership)
-- name: ListLapsedSubscriptionsWithEvidence :many
SELECT s.id, s.status, s.rail,
       (s.payment_method_id IS NOT NULL)::bool AS vaulted,
       s.rail_subscription_id,
       s.current_period_ends_at, s.grace_ends_at, s.next_retry_at, s.retry_attempts,
       (s.current_period_starts_at IS NOT NULL AND EXISTS (
            SELECT 1 FROM openrails.payments p
            WHERE p.subscription_id = s.id AND p.merchant_id = s.merchant_id
              AND p.status = 'completed'
              AND p.purchased_at >= s.current_period_starts_at
       ))::bool AS payment_opened_period,
       (s.current_period_ends_at IS NOT NULL AND EXISTS (
            SELECT 1 FROM openrails.payments p
            WHERE p.subscription_id = s.id AND p.merchant_id = s.merchant_id
              AND p.status = 'completed'
              AND p.purchased_at >= s.current_period_ends_at
       ))::bool AS renewal_payment_after_end,
       (s.current_period_ends_at IS NOT NULL AND EXISTS (
            SELECT 1 FROM openrails.rail_refresh_watermarks w
            WHERE w.merchant_id = s.merchant_id
              AND w.provider = s.rail
              AND (w.rail_merchant_account_id IS NULL OR w.rail_merchant_account_id = s.rail_merchant_account_id)
              AND w.watermark_at > s.current_period_ends_at
       ))::bool AS watermark_newer_than_period_end
FROM openrails.subscriptions s
WHERE s.merchant_id = sqlc.arg(merchant_id)::uuid
  AND (sqlc.narg(customer_id)::uuid IS NULL OR s.customer_id = sqlc.narg(customer_id)::uuid)
  AND (
        (s.status = 'active' AND s.current_period_ends_at IS NOT NULL
         AND s.current_period_ends_at < sqlc.arg(now)::timestamptz)
     OR (s.status = 'past_due' AND s.grace_ends_at IS NOT NULL
         AND s.grace_ends_at < sqlc.arg(now)::timestamptz AND s.next_retry_at IS NULL)
      )
ORDER BY s.current_period_ends_at;

-- #511 LIFE plane (life.subscription.pending_stale): pending subscriptions that
-- never confirmed within the threshold (cutoff = now - pendingStaleAfter).
-- name: ListStalePendingSubscriptions :many
SELECT id FROM openrails.subscriptions
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND (sqlc.narg(customer_id)::uuid IS NULL OR customer_id = sqlc.narg(customer_id)::uuid)
  AND status = 'pending'
  AND created_at < sqlc.arg(cutoff)::timestamptz
ORDER BY created_at;

-- #511 LIFE plane (life.provider_intent.abandoned): desired provider actions that
-- will not auto-retry (terminal/expired, or past their deadline) and need an
-- operator/admin. Surface-only (no auto-repair). Scoped by merchant (+ optional sub).
-- name: ListAbandonedProviderIntents :many
SELECT id, intent_type, status, provider FROM openrails.rail_intents
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND (sqlc.narg(subscription_id)::uuid IS NULL OR subscription_id = sqlc.narg(subscription_id)::uuid)
  AND (
        status IN ('failed_terminal', 'expired')
        OR (status IN ('pending', 'failed_retryable', 'unknown_needs_verify')
            AND expires_at IS NOT NULL AND expires_at < sqlc.arg(now)::timestamptz)
      )
ORDER BY created_at;

-- #632/#633 resolver: the `unknown` cohort awaiting provider verification, oldest
-- period first, bounded per call so provider-pull (#633) windows them in batches.
-- NULLS FIRST (#665): NULL-period rows (legacy imports without local period
-- evidence) are resolvable only here — via the roster/per-sub probe — so they
-- must never starve behind a large dated cohort under the LIMIT.
-- name: ListUnknownSubscriptions :many
SELECT id, rail, current_period_ends_at, rail_subscription_id FROM openrails.subscriptions
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND (sqlc.narg(customer_id)::uuid IS NULL OR customer_id = sqlc.narg(customer_id)::uuid)
  AND status = 'unknown'
  AND (sqlc.narg(rail)::text IS NULL OR rail = sqlc.narg(rail)::text)
ORDER BY current_period_ends_at ASC NULLS FIRST
LIMIT sqlc.arg(max_rows)::int;

-- #511 LIFE plane (life.subscription.dunning_overdue): a past_due sub still in
-- grace but with NO retry scheduled — its dunning schedule stalled. MISSING.
-- name: ListDunningStalledSubscriptions :many
SELECT id FROM openrails.subscriptions
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND (sqlc.narg(customer_id)::uuid IS NULL OR customer_id = sqlc.narg(customer_id)::uuid)
  AND status = 'past_due'
  AND next_retry_at IS NULL
  AND (grace_ends_at IS NULL OR grace_ends_at > sqlc.arg(now)::timestamptz)
ORDER BY current_period_ends_at;

-- name: SetSubscriptionNextRetry :execrows
-- Repair for dunning_overdue: re-establish the retry schedule so the dunning
-- worker resumes (a CURRENT retry within grace — not a replay of missed cycles).
UPDATE openrails.subscriptions
SET next_retry_at = sqlc.arg(next_retry_at)::timestamptz, updated_at = now()
WHERE id = sqlc.arg(id) AND status = 'past_due' AND next_retry_at IS NULL;

-- #665 DERIVE `derive.grant_effect.mismatch` (grant direction) — moved from the
-- legacy pull engine's PS-9. An `active` sub in a RUNNING period whose product
-- promises entitlements, where some promised feature was NEVER projected for
-- this period (no subscription-sourced window — live OR revoked — overlapping
-- it; a recorded revoke is a recorded decision, never re-granted, spec §6).
-- Excludes no-grant subs (owned by derive.subscription.missing) so the two
-- checks never double-fire. #695 absent-by-overlap: a LIVE window of ANY source
-- overlapping the running period also counts as projected — derive-2 would
-- deliberately project NO window over it (the repair records provenance only),
-- so detection must mirror the repair's no-op or the finding re-fires forever.
-- Returns the missing features as a spec blob so the repair
-- (grants.DeriveSubscriptionGrant) derives ONLY those. customer_id
-- nullable: NULL = merchant-wide sweep.
-- name: ListActiveSubsMissingEntitlementProjection :many
SELECT s.id, s.customer_id, s.product_id, s.status,
       s.current_period_starts_at, s.current_period_ends_at, s.started_at, s.ended_at,
       missing.spec AS entitlements_spec
FROM openrails.subscriptions s
JOIN openrails.products pd ON pd.id = s.product_id AND pd.merchant_id = s.merchant_id
CROSS JOIN LATERAL (
    SELECT jsonb_object_agg(feat, NULL::text) AS spec
    FROM jsonb_object_keys(pd.entitlements_spec) AS feat
    WHERE NOT EXISTS (
        SELECT 1 FROM openrails.entitlements e
        WHERE e.merchant_id = s.merchant_id
          AND e.source_type = 'subscription' AND e.source_id = s.id
          AND e.entitlement = feat
          AND e.deleted_at IS NULL
          AND e.start_at < s.current_period_ends_at
          AND (e.end_at IS NULL OR e.end_at > COALESCE(s.current_period_starts_at, s.started_at))
    )
    AND NOT EXISTS (
        SELECT 1 FROM openrails.entitlements eo
        WHERE eo.merchant_id = s.merchant_id
          AND eo.customer_id = s.customer_id
          AND eo.entitlement = feat
          AND eo.revoked_at IS NULL AND eo.deleted_at IS NULL
          AND eo.period && tstzrange(COALESCE(s.current_period_starts_at, s.started_at), s.current_period_ends_at, '[)')
    )
) missing
WHERE s.merchant_id = sqlc.arg(merchant_id)::uuid
  AND (sqlc.narg(customer_id)::uuid IS NULL OR s.customer_id = sqlc.narg(customer_id)::uuid)
  AND s.status = 'active'
  AND pd.entitlements_spec IS NOT NULL AND pd.entitlements_spec <> '{}'::jsonb
  AND s.current_period_ends_at IS NOT NULL AND s.current_period_ends_at > sqlc.arg(now)::timestamptz
  AND COALESCE(s.current_period_starts_at, s.started_at) <= sqlc.arg(now)::timestamptz
  AND EXISTS (
      SELECT 1 FROM openrails.grants g
      WHERE g.merchant_id = s.merchant_id AND g.event = 'grant'
        AND g.source_type = 'subscription' AND g.source_id = s.id::text
  )
  AND missing.spec IS NOT NULL
ORDER BY s.current_period_ends_at;

-- #665 DERIVE `derive.grant_effect.mismatch` (revoke direction) — moved from
-- the legacy pull engine's PS-9. A terminally-dead sub still projecting a
-- BOUNDED live window past its entitled bound: propagation of a recorded
-- terminal decision, AUTO (both facts present — NOT the confirmed-absence
-- case). `unknown` is deliberately excluded: access stays intact while
-- provider verification is pending (#664).
--
-- #690/#691 paid-through guard: the entitled bound is
-- GREATEST(current_period_ends_at, ended_at) — a user cancel leaves a PAID
-- RUNWAY window bounded to period end (BoundSubscriptionAccess), which is NOT
-- excess; only the part of a window extending past the bound is. Repair =
-- BoundSubscriptionAccess(sub, bound) — the missed/correct #691 closure.
-- Both timestamps NULL (imported oddity) => NULL bound, any live bounded
-- window counts and the repair bounds at `now`.
--
-- Partition (#690, one condition = one finding type):
--   bounded window past the bound, sub row present  -> HERE (AUTO closure)
--   STANDING window (end_at NULL) of a terminal sub -> derive.entitlement.unjustified (ADMIN)
--   sub row missing entirely                        -> derive.entitlement.unjustified (ADMIN)
--   terminated GRANT with a live window             -> derive.grant_effect.excess (AUTO)
-- customer_id nullable: NULL = merchant-wide sweep.
-- name: ListDeadSubsWithLiveEntitlements :many
SELECT s.id, s.customer_id, s.status, s.current_period_ends_at, s.ended_at
FROM openrails.subscriptions s
WHERE s.merchant_id = sqlc.arg(merchant_id)::uuid
  AND (sqlc.narg(customer_id)::uuid IS NULL OR s.customer_id = sqlc.narg(customer_id)::uuid)
  AND s.status IN ('cancelled', 'expired', 'failed')
  AND EXISTS (
      SELECT 1 FROM openrails.entitlements e
      WHERE e.merchant_id = s.merchant_id
        AND e.source_type = 'subscription' AND e.source_id = s.id
        AND e.revoked_at IS NULL AND e.deleted_at IS NULL
        AND e.end_at IS NOT NULL
        AND e.end_at > sqlc.arg(now)::timestamptz
        AND (GREATEST(s.current_period_ends_at, s.ended_at) IS NULL
             OR e.end_at > GREATEST(s.current_period_ends_at, s.ended_at))
  )
ORDER BY s.id;

-- #690 DERIVE `derive.entitlement.unjustified` — the FREELOADER detector
-- (renamed from derive.entitlement.orphan in migration 066: "orphaned" is
-- reserved for the paying-without-access category). A LIVE
-- window (not revoked/deleted, started, unbounded or ending in the future)
-- whose justification chain is PROVEN broken. Post-#691 fail-open, "live
-- window past paid-through" is NORMAL for a standing auto-renew projection
-- (stale ≠ freeloader) — a freeloader's SOURCE is proven dead or absent:
--   missing_subscription           - source_type=subscription, no sub row at all
--   terminal_subscription_standing - STANDING (end_at NULL) window whose sub is
--                                    terminal: the #691 closure event should
--                                    have bounded it and never did
--   refunded_payment               - one_off window whose payment was refunded,
--                                    with no live grant justifying the access
-- Grant-justification guard: never fires when a live un-terminated entitlement
-- grant covers now (matches MaterializeGrant's standing-access projection:
-- per-period grants of a live sub lapse while the standing window persists —
-- that is verification pressure, not freeloading), and never when the window's
-- backing grant is TERMINATED (derive.grant_effect.excess owns that
-- retraction). Non-live windows with dangling sub sources stay with
-- consistency.reference.source_reference. ADMIN surface-only — revoking access
-- is an operator decision, never auto (policy, #690).
--
-- Verification SQL (2026-07-01 doujins analysis, measured ZERO on the full
-- re-import): (1) grant-justification by source — live windows LEFT JOIN live
-- grants on (customer, source) counting NULLs per source_type; (2)
-- window-vs-paid-through by status — live windows joined to subscriptions
-- grouped by status comparing end_at against GREATEST(current_period_ends_at,
-- ended_at). This query is the union of both, restricted to proven-dead
-- sources. customer_id nullable: NULL = merchant-wide sweep.
-- name: ListUnjustifiedEntitlementWindows :many
SELECT e.id AS entitlement_id, e.customer_id, e.entitlement,
       e.source_type, e.source_id, e.start_at, e.end_at,
       COALESCE(s.status::text, '') AS sub_status,
       s.product_id AS sub_product_id,
       s.current_period_ends_at AS sub_period_ends_at,
       s.ended_at AS sub_ended_at,
       pay.id AS payment_id,
       pr.product_id AS payment_product_id,
       CASE
           WHEN e.source_type = 'subscription' AND s.id IS NULL THEN 'missing_subscription'
           WHEN e.source_type = 'subscription' THEN 'terminal_subscription_standing'
           ELSE 'refunded_payment'
       END::text AS cause
FROM openrails.entitlements e
LEFT JOIN openrails.subscriptions s
       ON e.source_type = 'subscription' AND s.id = e.source_id AND s.merchant_id = e.merchant_id
LEFT JOIN openrails.payments pay
       ON e.source_type = 'one_off' AND pay.id = e.source_id AND pay.merchant_id = e.merchant_id
LEFT JOIN openrails.prices pr
       ON pr.id = pay.price_id AND pr.merchant_id = e.merchant_id
WHERE e.merchant_id = sqlc.arg(merchant_id)::uuid
  AND (sqlc.narg(customer_id)::uuid IS NULL OR e.customer_id = sqlc.narg(customer_id)::uuid)
  AND e.revoked_at IS NULL AND e.deleted_at IS NULL
  AND e.start_at <= sqlc.arg(now)::timestamptz
  AND (e.end_at IS NULL OR e.end_at > sqlc.arg(now)::timestamptz)
  AND e.source_type IN ('subscription', 'one_off')
  -- no live un-terminated entitlement grant covering now justifies the window
  AND NOT EXISTS (
      SELECT 1 FROM openrails.grants g
      WHERE g.merchant_id = e.merchant_id
        AND g.customer_id = e.customer_id
        AND g.event = 'grant' AND g.kind = 'entitlement'
        AND (g.id = e.grant_id
             OR (g.source_id = e.source_id::text
                 AND ((e.source_type = 'subscription' AND g.source_type = 'subscription')
                      OR (e.source_type = 'one_off' AND g.source_type = 'purchase'))))
        AND g.starts_at <= sqlc.arg(now)::timestamptz
        AND (g.ends_at IS NULL OR g.ends_at > sqlc.arg(now)::timestamptz)
        AND NOT EXISTS (
            SELECT 1 FROM openrails.grants t
            WHERE t.merchant_id = g.merchant_id AND t.supersedes_id = g.id
              AND t.event IN ('revoke', 'expire', 'supersede'))
  )
  -- a TERMINATED backing grant means derive.grant_effect.excess owns the retraction
  AND NOT EXISTS (
      SELECT 1 FROM openrails.grants tg
      JOIN openrails.grants t ON t.merchant_id = tg.merchant_id AND t.supersedes_id = tg.id
                             AND t.event IN ('revoke', 'expire', 'supersede')
      WHERE tg.merchant_id = e.merchant_id AND tg.id = e.grant_id
  )
  AND (
      (e.source_type = 'subscription' AND s.id IS NULL)
      OR (e.source_type = 'subscription'
          AND s.status IN ('cancelled', 'expired', 'failed')
          AND e.end_at IS NULL)
      OR (e.source_type = 'one_off' AND pay.id IS NOT NULL AND pay.status = 'refunded')
  )
ORDER BY e.id;

-- #690/#691 `verification_pressure` gauge input: subscriptions parked (or
-- stuck) in `unknown` whose recorded paid-through has passed — standing access
-- awaiting provider verification. A pressure reading over the LIVE table, not
-- an error count: nonzero is allowed; max_age trending UP means the
-- verification machinery (pull/probe/converge) is down.
-- name: CountUnknownSubsPastPaidThrough :one
SELECT COUNT(*)::bigint AS pressure_count,
       COALESCE(MAX(EXTRACT(EPOCH FROM (sqlc.arg(now)::timestamptz - s.current_period_ends_at)))::bigint, 0) AS max_age_seconds
FROM openrails.subscriptions s
WHERE s.merchant_id = sqlc.arg(merchant_id)::uuid
  AND s.status = 'unknown'
  AND s.current_period_ends_at IS NOT NULL
  AND s.current_period_ends_at < sqlc.arg(now)::timestamptz;

-- #690 episode analytics totals: one compact pass over the two episode views
-- (migration 067) for the gauges header. Freeloader episodes split out the
-- `unsanctioned` cause (the failure class — sanctioned_dunning and
-- awaiting_verification are policy, never failure); open = the span still
-- accrues at now(). Days carry the views' documented approximations
-- (paid-through snapshot, uncovered-tail-only measurement).
-- name: CountErrorEpisodeTotals :one
SELECT fl.total::bigint            AS freeloader_total,
       fl.open_count::bigint       AS freeloader_open,
       fl.unsanctioned::bigint     AS freeloader_unsanctioned,
       fl.days::double precision   AS freeloader_days,
       o.total::bigint             AS orphaned_total,
       o.open_count::bigint        AS orphaned_open,
       o.days::double precision    AS orphaned_days
FROM (SELECT count(*) AS total,
             count(*) FILTER (WHERE open) AS open_count,
             count(*) FILTER (WHERE cause = 'unsanctioned') AS unsanctioned,
             COALESCE(sum(days), 0) AS days
        FROM openrails.freeloader_episodes
       WHERE merchant_id = sqlc.arg(merchant_id)::uuid) fl
CROSS JOIN (SELECT count(*) AS total,
                   count(*) FILTER (WHERE open) AS open_count,
                   COALESCE(sum(days), 0) AS days
              FROM openrails.orphaned_episodes
             WHERE merchant_id = sqlc.arg(merchant_id)::uuid) o;

-- #511 Phase E (Converge sweep worker): the privileged, no-GUC list of merchants
-- to sweep. merchants is a GLOBAL control-plane table (not RLS-scoped).
-- name: ListActiveMerchantIDs :many
SELECT id FROM openrails.merchants
WHERE status = 'active' AND deleted_at IS NULL
ORDER BY id;
