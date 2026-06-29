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

-- PS-10 stuck-intent findings are provider-independent (the subject is the
-- intent ledger; the finding carries the intent's own provider), so their
-- vanish sweep crosses providers: any actionable finding of the given types not
-- refreshed by the just-completed run recovered (the intent reached a
-- terminal-good status or no longer meets the stuck criteria).
-- name: AutoResolveVanishedReconciliationFindingsAllProviders :execrows
UPDATE openrails.reconciliation_findings
SET status = 'fixed',
    resolution = 'auto_vanished',
    resolved_at = now(),
    updated_at = now()
WHERE status IN ('reconcile_required', 'requires_review')
  AND last_seen_run <> sqlc.arg(run_id)
  AND finding_type = ANY (sqlc.arg(finding_types)::text[]);

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
  AND (sqlc.narg(provider_account_id)::uuid IS NULL OR provider_account_id = sqlc.narg(provider_account_id)::uuid);

-- name: ReconcileListPaymentsByTransactionIDs :many
SELECT id, customer_id, rail, transaction_id, amount, status,
       subscription_id, refunded_payment_id, purchased_at
FROM openrails.payments
WHERE rail::text = ANY (sqlc.arg(rails)::text[])
  AND transaction_id = ANY (sqlc.arg(transaction_ids)::text[])
  AND (sqlc.narg(provider_account_id)::uuid IS NULL OR provider_account_id = sqlc.narg(provider_account_id)::uuid);

-- Live subscription-sourced entitlements for the provider's subscriptions.
-- Grace windows and admin grants are deliberately excluded: only
-- subscription-sourced entitlements are reconciled (PS-9).
-- name: ReconcileListSubscriptionEntitlements :many
SELECT ent.id, ent.customer_id, ent.entitlement, ent.source_id,
       ent.start_at, ent.end_at
FROM openrails.entitlements ent
JOIN openrails.subscriptions sub ON sub.id = ent.source_id
WHERE sub.rail = ANY (sqlc.arg(rails)::text[])
  AND (sqlc.narg(provider_account_id)::uuid IS NULL OR sub.provider_account_id = sqlc.narg(provider_account_id)::uuid)
  AND ent.source_type = 'subscription'
  AND ent.revoked_at IS NULL
  AND ent.deleted_at IS NULL;

-- name: ReconcileListPaymentMethodsByRails :many
-- Reconcile is NMI-vault-specific: rail_customer_ref IS the NMI customer_vault_id
-- here, aliased to vault_id so the reconcile matcher keeps its NMI-vault vocabulary.
SELECT id, customer_id, rail, rail_customer_ref AS vault_id, last_four, card_type,
       expiry_date
FROM openrails.payment_methods
WHERE rail = ANY (sqlc.arg(rails)::text[])
  AND (sqlc.narg(provider_account_id)::uuid IS NULL OR provider_account_id = sqlc.narg(provider_account_id)::uuid);

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

-- PS-2: the rail says this subscription is dead -> cancel locally.
-- Clears the retry schedule (chk_cancelled_no_retry_schedule) and revokes via
-- the companion entitlement query. 0 rows on a re-run = already converged.
-- name: ReconcileCancelSubscriptionLocal :execrows
UPDATE openrails.subscriptions
SET status = 'cancelled',
    cancelled_at = COALESCE(cancelled_at, sqlc.arg(now)::timestamptz),
    cancel_type = COALESCE(cancel_type, sqlc.arg(cancel_type)::text),
    cancel_feedback = COALESCE(cancel_feedback, sqlc.arg(reason)::text),
    ended_at = COALESCE(ended_at, sqlc.arg(now)::timestamptz),
    next_retry_at = NULL,
    grace_ends_at = NULL,
    deletion_scheduled_at = NULL,
    updated_at = now()
WHERE id = sqlc.arg(id)
  AND status IN ('active', 'past_due', 'pending');

-- PS-3: adopt the rail's declared status/periods. Only non-terminal
-- targets route here (terminal remote states go through the cancel applier);
-- past_due adoption requires a period end (chk_past_due_has_period_end),
-- guarded by the engine.
-- name: ReconcileAdoptSubscriptionStatus :execrows
UPDATE openrails.subscriptions
SET status = sqlc.arg(status)::openrails.subscription_status,
    current_period_starts_at = COALESCE(sqlc.narg(period_starts_at)::timestamptz, current_period_starts_at),
    current_period_ends_at = COALESCE(sqlc.narg(period_ends_at)::timestamptz, current_period_ends_at),
    updated_at = now()
WHERE id = sqlc.arg(id)
  AND status <> 'cancelled'
  AND (status <> sqlc.arg(status)::openrails.subscription_status
       OR current_period_ends_at IS DISTINCT FROM COALESCE(sqlc.narg(period_ends_at)::timestamptz, current_period_ends_at));

-- PS-2/PS-9: revoke the LIVE subscription-sourced entitlements of one
-- subscription. Admin grants and grace windows are different source types and
-- are untouchable by construction.
-- name: ReconcileRevokeSubscriptionEntitlements :execrows
UPDATE openrails.entitlements
SET revoked_at = sqlc.arg(now)::timestamptz,
    revoke_reason = sqlc.arg(reason)::text,
    updated_at = now()
WHERE source_type = 'subscription'
  AND source_id = sqlc.arg(subscription_id)
  AND revoked_at IS NULL
  AND deleted_at IS NULL
  AND (end_at IS NULL OR end_at > sqlc.arg(now)::timestamptz);

-- PS-9/PS-4: grant one subscription-sourced entitlement window unless an
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
    status, subscription_id, metadata, purchased_at, customer_id, provider_account_id
) VALUES (
    sqlc.arg(merchant_id)::uuid,
    sqlc.arg(price_id), sqlc.arg(rail)::openrails.rail_type,
    sqlc.arg(transaction_id), sqlc.arg(amount), sqlc.arg(amount),
    sqlc.arg(currency),
    'completed', sqlc.narg(subscription_id), sqlc.narg(metadata),
    COALESCE(NULLIF(sqlc.arg(purchased_at)::timestamptz, '0001-01-01 00:00:00+00'::timestamptz), now()),
    sqlc.arg(customer_id), sqlc.narg(provider_account_id)
)
ON CONFLICT DO NOTHING;

-- PS-5: record a rail refund that is missing locally as a negative-
-- amount payment row linked to the refunded payment. Same dedupe identity.
-- name: ReconcileRecordRefund :execrows
INSERT INTO openrails.payments (
    merchant_id, price_id, rail, transaction_id, amount, list_amount, currency,
    status, subscription_id, refunded_payment_id, metadata, purchased_at,
    customer_id, provider_account_id
) VALUES (
    sqlc.arg(merchant_id)::uuid,
    sqlc.arg(price_id), sqlc.arg(rail)::openrails.rail_type,
    sqlc.arg(transaction_id), sqlc.arg(amount), sqlc.arg(amount),
    sqlc.arg(currency),
    'completed', sqlc.narg(subscription_id), sqlc.narg(refunded_payment_id),
    sqlc.narg(metadata),
    COALESCE(NULLIF(sqlc.arg(purchased_at)::timestamptz, '0001-01-01 00:00:00+00'::timestamptz), now()),
    sqlc.arg(customer_id), sqlc.narg(provider_account_id)
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
    entitlements_spec_snapshot, credits_spec_snapshot, customer_id, provider_account_id
)
SELECT sqlc.arg(merchant_id)::uuid, pr.id, pr.product_id, sqlc.arg(status)::openrails.subscription_status,
       sqlc.arg(rail), sqlc.arg(rail_subscription_id),
       sqlc.narg(user_email),
       sqlc.narg(period_starts_at)::timestamptz,
       sqlc.narg(period_ends_at)::timestamptz,
       COALESCE(sqlc.narg(started_at)::timestamptz, now()),
       p.entitlements_spec, p.credits_spec, sqlc.arg(customer_id), sqlc.narg(provider_account_id)
FROM openrails.prices pr
JOIN openrails.products p ON p.id = pr.product_id
WHERE pr.id = sqlc.arg(price_id)
  AND NOT EXISTS (
      SELECT 1 FROM openrails.subscriptions s
      WHERE s.rail_subscription_id = sqlc.arg(rail_subscription_id)
        AND s.rail = ANY (sqlc.arg(rails)::text[])
        AND (sqlc.narg(provider_account_id)::uuid IS NULL OR s.provider_account_id = sqlc.narg(provider_account_id)::uuid)
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

-- #511 LIFE plane (life.subscription.grace_exhausted): past_due subscriptions
-- whose grace window has elapsed — they should be terminal. Scope-aware detection.
-- name: ListGraceExhaustedSubscriptions :many
SELECT id, customer_id, grace_ends_at FROM openrails.subscriptions
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND (sqlc.narg(customer_id)::uuid IS NULL OR customer_id = sqlc.narg(customer_id)::uuid)
  AND status = 'past_due'
  AND grace_ends_at IS NOT NULL
  AND grace_ends_at < sqlc.arg(now)::timestamptz
ORDER BY grace_ends_at;

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
SELECT id, intent_type, status, provider FROM openrails.provider_intents
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND (sqlc.narg(subscription_id)::uuid IS NULL OR subscription_id = sqlc.narg(subscription_id)::uuid)
  AND (
        status IN ('failed_terminal', 'expired')
        OR (status IN ('pending', 'failed_retryable', 'unknown_needs_verify')
            AND expires_at IS NOT NULL AND expires_at < sqlc.arg(now)::timestamptz)
      )
ORDER BY created_at;

-- #511 LIFE plane (life.subscription.period_overdue): an `active` sub past its
-- current_period_ends_at that never advanced (missed rebill/failure webhook).
-- name: ListPeriodOverdueSubscriptions :many
SELECT id, current_period_ends_at FROM openrails.subscriptions
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND (sqlc.narg(customer_id)::uuid IS NULL OR customer_id = sqlc.narg(customer_id)::uuid)
  AND status = 'active'
  AND current_period_ends_at IS NOT NULL
  AND current_period_ends_at < sqlc.arg(now)::timestamptz
ORDER BY current_period_ends_at;

-- #511: the period_overdue repair (active → past_due + grace window) now routes
-- through subscriptions.SubscriptionLifecycleService.ApplyLocalPastDue (the shared
-- lifecycle local-state core), so there is no bespoke SQL applier here. The
-- ReconcileCancelSubscriptionLocal / ReconcileRevokeSubscriptionEntitlements
-- appliers above remain: they are the PULL engine's local writers (apply provider
-- truth), NOT the LIFE-plane's twin.

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

-- #511 Phase E (Converge sweep worker): the privileged, no-GUC list of merchants
-- to sweep. merchants is a GLOBAL control-plane table (not RLS-scoped).
-- name: ListActiveMerchantIDs :many
SELECT id FROM openrails.merchants
WHERE status = 'active' AND deleted_at IS NULL
ORDER BY id;
