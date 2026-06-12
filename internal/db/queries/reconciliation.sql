-- #107 phase 2: reconciliation runs + findings persistence, the engine's
-- tenant-scoped local-state reads, and the enforce appliers' idempotent local
-- writes. tenant_id is stamped explicitly (multi-tenant writer pattern); all
-- statements run on a tenant-pinned connection so RLS double-checks the stamp.

-- ============================================================================
-- Run lifecycle
-- ============================================================================

-- name: CreateReconciliationRun :one
INSERT INTO billing.reconciliation_runs (
    tenant_id, mode, providers, window_since, window_until, started_at, status
) VALUES (
    sqlc.arg(tenant_id), sqlc.arg(mode), sqlc.arg(providers),
    sqlc.narg(window_since), sqlc.narg(window_until), now(), 'running'
)
RETURNING *;

-- name: FinishReconciliationRun :execrows
UPDATE billing.reconciliation_runs
SET status = sqlc.arg(status),
    summary = sqlc.narg(summary),
    error = sqlc.narg(error),
    finished_at = now()
WHERE id = sqlc.arg(id) AND status = 'running';

-- name: GetReconciliationRun :one
SELECT * FROM billing.reconciliation_runs WHERE id = $1;

-- name: GetLatestReconciliationRun :one
SELECT * FROM billing.reconciliation_runs
ORDER BY started_at DESC
LIMIT 1;

-- name: ListReconciliationRuns :many
SELECT * FROM billing.reconciliation_runs
ORDER BY started_at DESC
LIMIT sqlc.arg(page_limit) OFFSET sqlc.arg(page_offset);

-- ============================================================================
-- Findings: stable-identity upsert + lifecycle
-- ============================================================================

-- Re-runs UPDATE the standing finding for (tenant, provider, finding_type,
-- subject_key). A previously resolved/auto_fixed finding that reappears is
-- REOPENED with the freshly computed status; a dismissed finding stays
-- dismissed (an admin explicitly silenced this identity) but keeps counting
-- occurrences so the dismissal is auditable against reality.
-- name: UpsertReconciliationFinding :one
INSERT INTO billing.reconciliation_findings (
    tenant_id, provider, finding_type, subject_key, severity, status,
    requires_admin, recommended_action, local_evidence, remote_evidence,
    intent_evidence, first_seen_run, last_seen_run
) VALUES (
    sqlc.arg(tenant_id), sqlc.arg(provider), sqlc.arg(finding_type),
    sqlc.arg(subject_key), sqlc.arg(severity), sqlc.arg(status),
    sqlc.arg(requires_admin), sqlc.narg(recommended_action),
    sqlc.narg(local_evidence), sqlc.narg(remote_evidence),
    sqlc.narg(intent_evidence), sqlc.arg(run_id), sqlc.arg(run_id)
)
ON CONFLICT (tenant_id, provider, finding_type, subject_key) DO UPDATE SET
    severity = EXCLUDED.severity,
    status = CASE
        WHEN billing.reconciliation_findings.status = 'dismissed' THEN 'dismissed'
        ELSE EXCLUDED.status
    END,
    requires_admin = EXCLUDED.requires_admin,
    recommended_action = EXCLUDED.recommended_action,
    local_evidence = EXCLUDED.local_evidence,
    remote_evidence = EXCLUDED.remote_evidence,
    intent_evidence = EXCLUDED.intent_evidence,
    resolution_evidence = CASE
        WHEN billing.reconciliation_findings.status = 'dismissed' THEN billing.reconciliation_findings.resolution_evidence
        ELSE NULL
    END,
    resolved_at = CASE
        WHEN billing.reconciliation_findings.status = 'dismissed' THEN billing.reconciliation_findings.resolved_at
        ELSE NULL
    END,
    resolution = CASE
        WHEN billing.reconciliation_findings.status = 'dismissed' THEN billing.reconciliation_findings.resolution
        ELSE NULL
    END,
    last_seen_run = EXCLUDED.last_seen_run,
    last_seen_at = now(),
    occurrence_count = billing.reconciliation_findings.occurrence_count + 1,
    updated_at = now()
RETURNING *;

-- name: GetReconciliationFinding :one
SELECT * FROM billing.reconciliation_findings WHERE id = $1;

-- name: ListReconciliationFindings :many
SELECT * FROM billing.reconciliation_findings
WHERE (sqlc.narg(status)::text IS NULL OR status = sqlc.narg(status)::text)
  AND (sqlc.narg(provider)::text IS NULL OR provider = sqlc.narg(provider)::text)
  AND (sqlc.narg(finding_type)::text IS NULL OR finding_type = sqlc.narg(finding_type)::text)
  AND (NOT sqlc.arg(only_admin_queue)::boolean OR (requires_admin AND status = 'admin_pending'))
ORDER BY last_seen_at DESC, id
LIMIT sqlc.arg(page_limit) OFFSET sqlc.arg(page_offset);

-- name: ListOpenReconciliationFindingsByProvider :many
SELECT * FROM billing.reconciliation_findings
WHERE provider = $1 AND status IN ('open', 'admin_pending')
ORDER BY finding_type, subject_key;

-- Findings of the given state-roster types absent from the just-completed run
-- covering their provider "vanished on their own" (design decision 1).
-- name: AutoResolveVanishedReconciliationFindings :execrows
UPDATE billing.reconciliation_findings
SET status = 'resolved',
    resolution = 'auto_vanished',
    resolved_at = now(),
    updated_at = now()
WHERE provider = sqlc.arg(provider)
  AND status IN ('open', 'admin_pending')
  AND last_seen_run <> sqlc.arg(run_id)
  AND finding_type = ANY (sqlc.arg(finding_types)::text[]);

-- PS-10 stuck-intent findings are provider-independent (the subject is the
-- intent ledger; the finding carries the intent's own provider), so their
-- vanish sweep crosses providers: any open finding of the given types not
-- refreshed by the just-completed run recovered (the intent reached a
-- terminal-good status or no longer meets the stuck criteria).
-- name: AutoResolveVanishedReconciliationFindingsAllProviders :execrows
UPDATE billing.reconciliation_findings
SET status = 'resolved',
    resolution = 'auto_vanished',
    resolved_at = now(),
    updated_at = now()
WHERE status IN ('open', 'admin_pending')
  AND last_seen_run <> sqlc.arg(run_id)
  AND finding_type = ANY (sqlc.arg(finding_types)::text[]);

-- name: MarkReconciliationFindingVanished :execrows
UPDATE billing.reconciliation_findings
SET status = 'resolved',
    resolution = 'auto_vanished',
    resolved_at = now(),
    updated_at = now()
WHERE id = sqlc.arg(id) AND status IN ('open', 'admin_pending');

-- name: MarkReconciliationFindingAutoFixed :execrows
UPDATE billing.reconciliation_findings
SET status = 'auto_fixed',
    resolution = 'enforced',
    resolution_evidence = sqlc.narg(resolution_evidence),
    resolved_at = now(),
    updated_at = now()
WHERE id = sqlc.arg(id) AND status IN ('open', 'admin_pending');

-- name: AckReconciliationFinding :execrows
UPDATE billing.reconciliation_findings
SET status = 'resolved',
    resolution = 'admin_ack',
    notes = sqlc.narg(notes),
    resolved_at = now(),
    updated_at = now()
WHERE id = sqlc.arg(id) AND status IN ('open', 'admin_pending', 'auto_fixed');

-- name: DismissReconciliationFinding :execrows
UPDATE billing.reconciliation_findings
SET status = 'dismissed',
    resolution = 'dismissed',
    notes = sqlc.narg(notes),
    resolved_at = now(),
    updated_at = now()
WHERE id = sqlc.arg(id) AND status IN ('open', 'admin_pending', 'auto_fixed');

-- ============================================================================
-- Local-state reads for the diff engine
-- ============================================================================

-- name: ReconcileListSubscriptionsByProcessors :many
SELECT id, tenant_subject_id, price_id, product_id, status, processor,
       processor_subscription_id, user_email, payment_method_id,
       current_period_starts_at, current_period_ends_at, started_at, ended_at,
       cancelled_at, cancel_type, deletion_scheduled_at, tier_group,
       last_retry_at, retry_attempts, next_retry_at,
       entitlements_spec_snapshot
FROM billing.subscriptions
WHERE processor = ANY (sqlc.arg(processors)::text[]);

-- name: ReconcileListPaymentsByTransactionIDs :many
SELECT id, tenant_subject_id, processor, transaction_id, amount, status,
       subscription_id, refunded_payment_id, purchased_at
FROM billing.payments
WHERE processor::text = ANY (sqlc.arg(processors)::text[])
  AND transaction_id = ANY (sqlc.arg(transaction_ids)::text[]);

-- Live subscription-sourced entitlements for the provider's subscriptions.
-- Grace windows and admin grants are deliberately excluded: only
-- subscription-sourced entitlements are reconciled (PS-9).
-- name: ReconcileListSubscriptionEntitlements :many
SELECT ent.id, ent.tenant_subject_id, ent.entitlement, ent.source_id,
       ent.start_at, ent.end_at
FROM billing.entitlements ent
JOIN billing.subscriptions sub ON sub.id = ent.source_id
WHERE sub.processor = ANY (sqlc.arg(processors)::text[])
  AND ent.source_type = 'subscription'
  AND ent.revoked_at IS NULL
  AND ent.deleted_at IS NULL;

-- name: ReconcileListPaymentMethodsByProcessors :many
SELECT id, tenant_subject_id, processor, vault_id, last_four, card_type,
       expiry_date
FROM billing.payment_methods
WHERE processor = ANY (sqlc.arg(processors)::text[]);

-- name: ReconcileListSolanaSubscriptionRefs :many
SELECT subscription_pda, plan_pda, subscriber_wallet
FROM billing.solana_subscriptions;

-- Billable prices with their processor link blobs (provider_links): the PS-1
-- materializer maps a remote plan id onto the local price whose processors
-- jsonb carries that id under the provider's key. Draft prices are excluded
-- (not billable); archived prices stay (grandfathered subscriptions bill them).
-- name: ReconcileListPricesWithProcessors :many
SELECT id, product_id, amount, currency, billing_cycle_days, status, processors
FROM billing.prices
WHERE processors IS NOT NULL
  AND status <> 'draft';

-- ============================================================================
-- Enforce appliers: idempotent LOCAL writes only (never a provider call)
-- ============================================================================

-- PS-2: the processor says this subscription is dead -> cancel locally.
-- Clears the retry schedule (chk_cancelled_no_retry_schedule) and revokes via
-- the companion entitlement query. 0 rows on a re-run = already converged.
-- name: ReconcileCancelSubscriptionLocal :execrows
UPDATE billing.subscriptions
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

-- PS-3: adopt the processor's declared status/periods. Only non-terminal
-- targets route here (terminal remote states go through the cancel applier);
-- past_due adoption requires a period end (chk_past_due_has_period_end),
-- guarded by the engine.
-- name: ReconcileAdoptSubscriptionStatus :execrows
UPDATE billing.subscriptions
SET status = sqlc.arg(status)::billing.subscription_status,
    current_period_starts_at = COALESCE(sqlc.narg(period_starts_at)::timestamptz, current_period_starts_at),
    current_period_ends_at = COALESCE(sqlc.narg(period_ends_at)::timestamptz, current_period_ends_at),
    updated_at = now()
WHERE id = sqlc.arg(id)
  AND status <> 'cancelled'
  AND (status <> sqlc.arg(status)::billing.subscription_status
       OR current_period_ends_at IS DISTINCT FROM COALESCE(sqlc.narg(period_ends_at)::timestamptz, current_period_ends_at));

-- PS-2/PS-9: revoke the LIVE subscription-sourced entitlements of one
-- subscription. Admin grants and grace windows are different source types and
-- are untouchable by construction.
-- name: ReconcileRevokeSubscriptionEntitlements :execrows
UPDATE billing.entitlements
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
INSERT INTO billing.entitlements (
    tenant_id, tenant_subject_id, entitlement, start_at, end_at,
    source_id, source_type
)
SELECT sqlc.arg(tenant_id), sqlc.arg(tenant_subject_id), sqlc.arg(entitlement),
       sqlc.arg(start_at)::timestamptz, sqlc.narg(end_at)::timestamptz,
       sqlc.arg(subscription_id), 'subscription'
WHERE NOT EXISTS (
    SELECT 1 FROM billing.entitlements ent
    WHERE ent.tenant_subject_id = sqlc.arg(tenant_subject_id)
      AND ent.entitlement = sqlc.arg(entitlement)
      AND ent.source_type = 'subscription'
      AND ent.source_id = sqlc.arg(subscription_id)
      AND ent.revoked_at IS NULL
      AND ent.deleted_at IS NULL
      AND (ent.end_at IS NULL OR ent.end_at > sqlc.arg(now)::timestamptz)
);

-- PS-4: backfill a processor charge that has no local payment record.
-- Dedupe rides the uq_payments_tenant_processor_transaction identity.
-- name: ReconcileBackfillPayment :execrows
INSERT INTO billing.payments (
    price_id, processor, transaction_id, amount, list_amount, currency,
    status, subscription_id, metadata, purchased_at, tenant_subject_id
) VALUES (
    sqlc.arg(price_id), sqlc.arg(processor)::billing.processor_type,
    sqlc.arg(transaction_id), sqlc.arg(amount), sqlc.arg(amount),
    COALESCE(NULLIF(sqlc.arg(currency)::text, ''), 'usd'),
    'completed', sqlc.narg(subscription_id), sqlc.narg(metadata),
    COALESCE(NULLIF(sqlc.arg(purchased_at)::timestamptz, '0001-01-01 00:00:00+00'::timestamptz), now()),
    sqlc.arg(tenant_subject_id)
)
ON CONFLICT (tenant_id, processor, transaction_id) DO NOTHING;

-- PS-5: record a processor refund that is missing locally as a negative-
-- amount payment row linked to the refunded payment. Same dedupe identity.
-- name: ReconcileRecordRefund :execrows
INSERT INTO billing.payments (
    price_id, processor, transaction_id, amount, list_amount, currency,
    status, subscription_id, refunded_payment_id, metadata, purchased_at,
    tenant_subject_id
) VALUES (
    sqlc.arg(price_id), sqlc.arg(processor)::billing.processor_type,
    sqlc.arg(transaction_id), sqlc.arg(amount), sqlc.arg(amount),
    COALESCE(NULLIF(sqlc.arg(currency)::text, ''), 'usd'),
    'completed', sqlc.narg(subscription_id), sqlc.narg(refunded_payment_id),
    sqlc.narg(metadata),
    COALESCE(NULLIF(sqlc.arg(purchased_at)::timestamptz, '0001-01-01 00:00:00+00'::timestamptz), now()),
    sqlc.arg(tenant_subject_id)
)
ON CONFLICT (tenant_id, processor, transaction_id) DO NOTHING;

-- name: ReconcileMarkPaymentRefunded :execrows
UPDATE billing.payments
SET status = 'refunded'
WHERE id = sqlc.arg(id) AND status <> 'refunded';

-- PS-1 materialization (bootstrap mode, --materialize): create the local
-- subscription for a processor subscription that resolved unambiguously to an
-- identity and a price. The entitlements/credits specs snapshot from the
-- product exactly like a normal signup, so the subscription-sourced
-- entitlement path works unchanged. Idempotent: a second run inserts nothing
-- when any subscription already carries the processor subscription id (zero
-- rows returned = already materialized).
-- name: ReconcileMaterializeSubscription :many
INSERT INTO billing.subscriptions (
    price_id, product_id, status, processor, processor_subscription_id,
    user_email, current_period_starts_at, current_period_ends_at, started_at,
    entitlements_spec_snapshot, credits_spec_snapshot, tenant_subject_id
)
SELECT pr.id, pr.product_id, sqlc.arg(status)::billing.subscription_status,
       sqlc.arg(processor), sqlc.arg(processor_subscription_id),
       sqlc.narg(user_email),
       sqlc.narg(period_starts_at)::timestamptz,
       sqlc.narg(period_ends_at)::timestamptz,
       COALESCE(sqlc.narg(started_at)::timestamptz, now()),
       p.entitlements_spec, p.credits_spec, sqlc.arg(tenant_subject_id)
FROM billing.prices pr
JOIN billing.products p ON p.id = pr.product_id
WHERE pr.id = sqlc.arg(price_id)
  AND NOT EXISTS (
      SELECT 1 FROM billing.subscriptions s
      WHERE s.processor_subscription_id = sqlc.arg(processor_subscription_id)
        AND s.processor = ANY (sqlc.arg(processors)::text[])
  )
RETURNING id, entitlements_spec_snapshot;

-- PS-7: adopt the processor's vault metadata for a stored payment method.
-- name: ReconcileAdoptPaymentMethod :execrows
UPDATE billing.payment_methods
SET last_four = COALESCE(NULLIF(sqlc.arg(last_four)::text, ''), last_four),
    expiry_date = COALESCE(NULLIF(sqlc.arg(expiry_date)::text, ''), expiry_date),
    updated_at = now()
WHERE id = sqlc.arg(id)
  AND (last_four IS DISTINCT FROM NULLIF(sqlc.arg(last_four)::text, '')
       OR expiry_date IS DISTINCT FROM NULLIF(sqlc.arg(expiry_date)::text, ''));
