-- #107 phase 2: reconciliation runs + findings persistence, the engine's
-- merchant-scoped local-state reads, and the enforce appliers' idempotent local
-- writes. merchant_id is stamped explicitly (multi-merchant writer pattern); all
-- statements run on a merchant-pinned connection.

-- ============================================================================
-- Run lifecycle
-- ============================================================================

-- name: CreateReconciliationRun :one
INSERT INTO billing.maintenance_runs (
    merchant_id, kind, mode, rails, window_starts_at, window_ends_at, started_at, status
) VALUES (
    sqlc.arg(merchant_id), 'reconciliation', sqlc.arg(mode)::text, sqlc.arg(rails),
    sqlc.narg(window_starts_at), sqlc.narg(window_ends_at), now(), 'running'
)
RETURNING *;

-- name: FinishReconciliationRun :execrows
UPDATE billing.maintenance_runs
SET status = sqlc.arg(status),
    summary = sqlc.narg(summary),
    error = sqlc.narg(error),
    finished_at = now()
WHERE id = sqlc.arg(id) AND kind='reconciliation' AND merchant_id=billing.current_merchant_id() AND status = 'running';

-- name: GetReconciliationRun :one
SELECT * FROM billing.maintenance_runs WHERE id = $1 AND kind='reconciliation' AND merchant_id=billing.current_merchant_id();

-- name: GetLatestReconciliationRun :one
SELECT * FROM billing.maintenance_runs
WHERE kind='reconciliation' AND merchant_id=billing.current_merchant_id()
ORDER BY started_at DESC
LIMIT 1;

-- name: ListReconciliationRuns :many
SELECT * FROM billing.maintenance_runs
WHERE kind='reconciliation' AND merchant_id=billing.current_merchant_id()
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
INSERT INTO billing.reconciliation_findings (
    merchant_id, finding_type, subject_key, severity, status,
    recommended_action, evidence, resolved_at, resolution,
    first_seen_run, last_seen_run, psp_id, rail
) VALUES (
    sqlc.arg(merchant_id), sqlc.arg(finding_type), sqlc.arg(subject_key),
    sqlc.arg(severity), sqlc.arg(status), sqlc.narg(recommended_action),
    sqlc.narg(evidence)::jsonb,
    CASE WHEN sqlc.arg(status)::text = 'auto_fixed' THEN now() ELSE NULL END,
    CASE WHEN sqlc.arg(status)::text = 'auto_fixed' THEN 'enforced' ELSE NULL END,
    sqlc.narg(run_id), sqlc.narg(run_id), sqlc.narg(psp_id)::uuid,
    -- A pull finding carries its PSP's rail (the psps FK checks they agree).
    (SELECT p.rail FROM billing.psps p WHERE p.merchant_id = sqlc.arg(merchant_id) AND p.id = sqlc.narg(psp_id)::uuid)
)
ON CONFLICT (merchant_id, finding_type, psp_id, subject_key) DO UPDATE SET
    severity = EXCLUDED.severity,
    status = CASE
        WHEN billing.reconciliation_findings.status = 'ignored' THEN 'ignored'
        ELSE EXCLUDED.status
    END,
    recommended_action = EXCLUDED.recommended_action,
    evidence = CASE
        WHEN billing.reconciliation_findings.status = 'ignored' THEN billing.reconciliation_findings.evidence
        ELSE EXCLUDED.evidence
    END,
    resolved_at = CASE
        WHEN billing.reconciliation_findings.status = 'ignored' THEN billing.reconciliation_findings.resolved_at
        WHEN EXCLUDED.status = 'auto_fixed' THEN EXCLUDED.resolved_at
        ELSE NULL
    END,
    resolution = CASE
        WHEN billing.reconciliation_findings.status = 'ignored' THEN billing.reconciliation_findings.resolution
        WHEN EXCLUDED.status = 'auto_fixed' THEN EXCLUDED.resolution
        ELSE NULL
    END,
    -- #787: an inline auto_fixed transition (the only resolution this upsert
    -- itself can produce; fixed/ignored come via the separate admin/auto-
    -- resolve statements below) is a resolution — clear the notify linkage so
    -- a future reopen of this identity notifies again.
    notified_at = CASE
        WHEN billing.reconciliation_findings.status = 'ignored' THEN billing.reconciliation_findings.notified_at
        WHEN EXCLUDED.status = 'auto_fixed' THEN NULL
        ELSE billing.reconciliation_findings.notified_at
    END,
    notified_severity = CASE
        WHEN billing.reconciliation_findings.status = 'ignored' THEN billing.reconciliation_findings.notified_severity
        WHEN EXCLUDED.status = 'auto_fixed' THEN NULL
        ELSE billing.reconciliation_findings.notified_severity
    END,
    last_seen_run = COALESCE(EXCLUDED.last_seen_run, billing.reconciliation_findings.last_seen_run),
    last_seen_at = now(),
    updated_at = now()
RETURNING *;

-- name: ClaimReconciliationFindingNotification :execrows
-- Claim one open episode/escalation in the same transaction as its notification.
UPDATE billing.reconciliation_findings
SET notified_at = sqlc.arg(notified_at)::timestamptz,
    notified_severity = sqlc.arg(severity)::text
WHERE reconciliation_findings.merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id)::uuid
  AND status = 'requires_review'
  AND severity = sqlc.arg(severity)::text
  AND (notified_at IS NULL OR
       array_position(ARRAY['critical','high','medium','low'], severity) <
       COALESCE(array_position(ARRAY['critical','high','medium','low'], notified_severity), 5));

-- name: MarkReconciliationFindingNotified :execrows
-- #787: dedupe linkage for the immediate notify path — set once a finding
-- pushes an operator notification, cleared by every resolution statement below
-- so a reopened finding notifies again.
UPDATE billing.reconciliation_findings
SET notified_at = sqlc.arg(notified_at)::timestamptz,
    notified_severity = sqlc.arg(severity)::text
WHERE reconciliation_findings.merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id);

-- SEC-18: the merchant predicate is this query's only merchant scope. This is a
-- merchant-admin by-id surface (GET /v1/admin/findings/:id, and the resolve
-- below EXECUTES cancel/refund/revoke/grant against whatever the finding
-- names); before this it was `WHERE id = $1`, which let merchant A's owner
-- address merchant B's finding on any connection the since-removed RLS did
-- not filter.
-- name: GetReconciliationFinding :one
SELECT * FROM billing.reconciliation_findings
WHERE id = $1 AND merchant_id = billing.current_merchant_id();

-- name: ListReconciliationFindings :many
SELECT * FROM billing.reconciliation_findings
WHERE reconciliation_findings.merchant_id = sqlc.arg(merchant_id)::uuid AND (sqlc.narg(status)::text IS NULL OR status = sqlc.narg(status)::text)
  AND (sqlc.narg(provider)::text IS NULL OR COALESCE(NULLIF(rail,''),evidence->>'provider') = sqlc.narg(provider)::text)
  AND (sqlc.narg(finding_type)::text IS NULL OR finding_type = sqlc.narg(finding_type)::text)
  AND (NOT sqlc.arg(only_review_queue)::boolean OR status = 'requires_review')
ORDER BY last_seen_at DESC, id
LIMIT sqlc.arg(page_limit) OFFSET sqlc.arg(page_offset);

-- name: ListActionablePullFindingsForPSP :many
SELECT * FROM billing.reconciliation_findings
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND psp_id = sqlc.arg(psp_id)::uuid
  AND finding_type LIKE 'pull.%' AND status IN ('reconcile_required', 'requires_review', 'ignored')
ORDER BY finding_type, subject_key;

-- Findings of the given state-roster types absent from the just-completed run
-- covering their provider "vanished on their own" (design decision 1).
-- or#837: batched and merchant-pinned. It used to be one unbounded UPDATE with
-- no merchant predicate at all — a long transaction on a big backlog, and a
-- cross-merchant write.
-- name: AutoResolveVanishedReconciliationFindings :execrows
UPDATE billing.reconciliation_findings
SET status = 'fixed',
    resolution = 'auto_vanished',
    resolved_at = now(),
    notified_at = NULL, notified_severity = NULL, -- #787: resolution clears the notify linkage
    updated_at = now()
WHERE ctid IN (
    SELECT f.ctid FROM billing.reconciliation_findings f
    WHERE f.merchant_id = sqlc.arg(merchant_id)::uuid
      AND f.psp_id = sqlc.arg(psp_id)::uuid
      AND f.status IN ('reconcile_required', 'requires_review', 'ignored')
      AND f.last_seen_run IS DISTINCT FROM sqlc.arg(run_id)
      AND f.finding_type = ANY (sqlc.arg(finding_types)::text[])
    LIMIT sqlc.arg(row_limit)::int
);

-- life.provider_intent.stuck findings recover subject-first: an open finding
-- whose intent no longer meets the stuck criteria (executed, superseded, or
-- re-scheduled) auto-resolves on the next LIFE pass. Cutoffs mirror the
-- detection (ListStuckProviderIntents) exactly — edit together.
-- name: AutoResolveRecoveredStuckIntentFindings :execrows
UPDATE billing.reconciliation_findings f
SET status = 'fixed',
    resolution = 'auto_vanished',
    resolved_at = now(),
    notified_at = NULL, notified_severity = NULL, -- #787: resolution clears the notify linkage
    updated_at = now()
WHERE f.merchant_id = sqlc.arg(merchant_id)::uuid
  AND f.finding_type = 'life.provider_intent.stuck'
  AND f.status IN ('reconcile_required', 'requires_review')
  AND NOT EXISTS (
      SELECT 1 FROM billing.provider_intents pi
      WHERE pi.merchant_id = f.merchant_id
        AND pi.id::text = f.subject_key
        AND ((pi.status IN ('pending', 'failed_retryable') AND pi.created_at <= sqlc.arg(action_cutoff)::timestamptz)
          OR (pi.status IN ('in_flight', 'unknown_needs_verify') AND pi.created_at <= sqlc.arg(verify_cutoff)::timestamptz))
  );

-- name: MarkReconciliationFindingVanished :execrows
UPDATE billing.reconciliation_findings
SET status = 'fixed',
    resolution = 'auto_vanished',
    resolved_at = now(),
    notified_at = NULL, notified_severity = NULL, -- #787: resolution clears the notify linkage
    updated_at = now()
WHERE reconciliation_findings.merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id) AND status IN ('reconcile_required', 'requires_review', 'ignored');

-- name: MarkReconciliationFindingAutoFixed :execrows
UPDATE billing.reconciliation_findings
SET status = 'auto_fixed',
    resolution = 'enforced',
    evidence = jsonb_set(COALESCE(evidence, '{}'::jsonb), '{resolution}', sqlc.narg(resolution_evidence)::jsonb, true),
    resolved_at = now(),
    notified_at = NULL, notified_severity = NULL, -- #787: resolution clears the notify linkage
    updated_at = now()
WHERE reconciliation_findings.merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id) AND status IN ('reconcile_required', 'requires_review');

-- name: AckReconciliationFinding :execrows
UPDATE billing.reconciliation_findings
SET status = 'fixed',
    resolution = 'admin_fixed',
    operator_notes = sqlc.narg(operator_notes),
    resolved_at = now(),
    notified_at = NULL, notified_severity = NULL, -- #787: resolution clears the notify linkage
    updated_at = now()
WHERE reconciliation_findings.merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id) AND status IN ('reconcile_required', 'requires_review', 'auto_fixed');

-- name: DismissReconciliationFinding :execrows
UPDATE billing.reconciliation_findings
SET status = 'ignored',
    resolution = 'ignored',
    operator_notes = sqlc.narg(operator_notes),
    resolved_at = now(),
    notified_at = NULL, notified_severity = NULL, -- #787: resolution clears the notify linkage
    updated_at = now()
WHERE reconciliation_findings.merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id) AND status IN ('reconcile_required', 'requires_review', 'auto_fixed', 'fixed');

-- ============================================================================
-- #692 operator findings queue (admin API)
-- ============================================================================

-- The operator work list. Default view = OPEN findings only; an explicit
-- status filter overrides it (e.g. status=ignored). Sort: severity desc
-- (critical first) then age desc (oldest first). total_count rides every row
-- for pagination. merchant_id stamped explicitly (multi-merchant pattern).
-- name: AdminListReconciliationFindings :many
SELECT f.*
FROM billing.reconciliation_findings f
WHERE f.merchant_id = sqlc.arg(merchant_id)::uuid
  AND (CASE
         WHEN sqlc.narg(status)::text IS NULL THEN f.status IN ('reconcile_required', 'requires_review')
         ELSE f.status = sqlc.narg(status)::text
       END)
  AND (sqlc.narg(severity)::text IS NULL OR f.severity = sqlc.narg(severity)::text)
  AND (sqlc.narg(finding_type)::text IS NULL OR f.finding_type = sqlc.narg(finding_type)::text)
  AND (sqlc.narg(type_prefix)::text IS NULL OR starts_with(f.finding_type, sqlc.narg(type_prefix)::text))
  AND (sqlc.narg(after_rank)::int IS NULL
       OR (CASE f.severity WHEN 'critical' THEN 0 WHEN 'high' THEN 1 WHEN 'medium' THEN 2 ELSE 3 END, f.created_at, f.id)
          > (sqlc.narg(after_rank)::int, sqlc.narg(after_at)::timestamptz, sqlc.narg(after_id)::uuid))
ORDER BY CASE f.severity WHEN 'critical' THEN 0 WHEN 'high' THEN 1 WHEN 'medium' THEN 2 ELSE 3 END,
         f.created_at, f.id
LIMIT sqlc.arg(row_limit)::int;

-- name: ListReconciliationFindingsByIDs :many
-- Named findings, open or resolved, in the work list's order.
SELECT f.*
FROM billing.reconciliation_findings f
WHERE f.merchant_id = sqlc.arg(merchant_id)::uuid AND f.id = ANY(sqlc.arg(ids)::uuid[])
ORDER BY CASE f.severity WHEN 'critical' THEN 0 WHEN 'high' THEN 1 WHEN 'medium' THEN 2 ELSE 3 END,
         f.created_at, f.id;

-- Approve: the recommendation executed; mark fixed/admin_fixed with operator
-- notes + attribution, and record the execution evidence under
-- evidence.resolution. Only OPEN findings resolve — approve on an already-
-- resolved finding is a handler-level 409.
-- name: AdminResolveReconciliationFinding :execrows
UPDATE billing.reconciliation_findings
SET status = 'fixed',
    resolution = 'admin_fixed',
    operator_notes = sqlc.narg(operator_notes),
    resolved_by = sqlc.arg(resolved_by),
    evidence = CASE
        WHEN sqlc.narg(resolution_evidence)::jsonb IS NULL THEN evidence
        ELSE jsonb_set(COALESCE(evidence, '{}'::jsonb), '{resolution}', sqlc.narg(resolution_evidence)::jsonb, true)
    END,
    resolved_at = now(),
    notified_at = NULL, notified_severity = NULL, -- #787: resolution clears the notify linkage
    updated_at = now()
WHERE id = sqlc.arg(id)
  AND merchant_id = billing.current_merchant_id() -- SEC-18: defence in depth, see GetReconciliationFinding
  AND status IN ('reconcile_required', 'requires_review');

-- Ignore: permanent silence for the subject (the upsert keeps ignored
-- identities ignored across re-runs — same semantics the breaker's dismiss
-- honors). Notes are REQUIRED (enforced at the handler).
-- name: AdminIgnoreReconciliationFinding :execrows
UPDATE billing.reconciliation_findings
SET status = 'ignored',
    resolution = 'ignored',
    operator_notes = sqlc.narg(operator_notes),
    resolved_by = sqlc.arg(resolved_by),
    resolved_at = now(),
    notified_at = NULL, notified_severity = NULL, -- #787: resolution clears the notify linkage
    updated_at = now()
WHERE id = sqlc.arg(id)
  AND merchant_id = billing.current_merchant_id() -- SEC-18: defence in depth, see GetReconciliationFinding
  AND status IN ('reconcile_required', 'requires_review');

-- Partial failure: append the execution error to operator_notes; the finding
-- STAYS OPEN (never half-marked fixed).
-- name: AppendReconciliationFindingNotes :execrows
UPDATE billing.reconciliation_findings
SET operator_notes = CASE
        WHEN COALESCE(operator_notes, '') = '' THEN sqlc.arg(note)::text
        ELSE operator_notes || E'\n' || sqlc.arg(note)::text
    END,
    updated_at = now()
WHERE id = sqlc.arg(id)
  AND merchant_id = billing.current_merchant_id() -- SEC-18: defence in depth, see GetReconciliationFinding
  AND status IN ('reconcile_required', 'requires_review');

-- ============================================================================
-- Local-state reads for the diff engine
-- ============================================================================

-- name: ReconcileListSubscriptionsByRails :many
SELECT subscriptions.id, subscriptions.customer_id, subscriptions.price_id, subscriptions.product_id,
       subscriptions.status, subscriptions.rail, subscriptions.collection_policy, subscriptions.rail_subscription_id, subscriptions.payment_method_id,
       subscriptions.current_period_starts_at, subscriptions.current_period_ends_at,
       subscriptions.started_at, subscriptions.ended_at, subscriptions.canceled_at, subscriptions.cancel_type,
       subscriptions.deletion_scheduled_at, subscriptions.tier_group, subscriptions.last_retry_at,
       subscriptions.retry_attempts, subscriptions.next_retry_at,
       subscriptions.access_duration_hours_snapshot,
       scheduled.price_id AS scheduled_price_id,
       price.currency AS price_currency,
       EXISTS (SELECT 1 FROM billing.provider_intents ri
               WHERE ri.merchant_id = subscriptions.merchant_id AND ri.subscription_id = subscriptions.id
                 AND ri.intent_type = 'nmi_upgrade'
                 AND ri.status IN ('pending', 'in_flight', 'unknown_needs_verify', 'failed_retryable'))::boolean AS tier_change_pending
FROM billing.subscriptions subscriptions
LEFT JOIN billing.prices price ON price.merchant_id = subscriptions.merchant_id AND price.id = subscriptions.price_id
LEFT JOIN billing.scheduled_changes scheduled ON scheduled.merchant_id = subscriptions.merchant_id
  AND scheduled.subscription_id = subscriptions.id AND scheduled.status = 'scheduled'
WHERE subscriptions.merchant_id = sqlc.arg(merchant_id)::uuid AND subscriptions.rail = ANY (sqlc.arg(rails)::text[])
  AND subscriptions.deleted_at IS NULL
  AND subscriptions.psp_id = sqlc.arg(psp_id)::uuid;

-- name: ReconcileListPaymentsByTransactionIDs :many
SELECT id, customer_id, rail, transaction_id, amount, currency, status,
       subscription_id, refunded_payment_id, purchased_at
FROM billing.payments
WHERE payments.merchant_id = sqlc.arg(merchant_id)::uuid AND rail::text = ANY (sqlc.arg(rails)::text[])
  AND deleted_at IS NULL
  AND transaction_id = ANY (sqlc.arg(transaction_ids)::text[])
  AND psp_id = sqlc.arg(psp_id)::uuid;

-- name: ReconcileListPaymentMethodsByRails :many
-- rail_customer_ref is the rail's handle on the stored instrument (on NMI it is
-- the customer_vault_id). or#871: no `AS vault_id` alias — `vault` is reserved
-- for HashiCorp Vault, and the column already carries the right name.
SELECT id, customer_id, rail, rail_customer_ref, rail_method_ref, card_brand, card_last4,
       card_exp_month, card_exp_year
FROM billing.payment_methods
WHERE payment_methods.merchant_id = sqlc.arg(merchant_id)::uuid AND rail = ANY (sqlc.arg(rails)::text[])
  AND psp_id = sqlc.arg(psp_id)::uuid;

-- name: ReconcileListSolanaSubscriptionRefs :many
SELECT subscription_pda, plan_pda, subscriber_wallet
FROM billing.solana_subscriptions
WHERE solana_subscriptions.merchant_id = sqlc.arg(merchant_id)::uuid
;

-- Billable prices with their rail link blobs (provider_links): the PS-1
-- materializer maps a remote plan id onto the local price whose psp_links
-- jsonb carries that id under the provider's key. Archived prices stay
-- (grandfathered subscriptions bill them).
-- name: ReconcileListPricesWithPSPLinks :many
SELECT id, product_id, amount, currency, access_duration_hours, billing_interval_hours, archived
FROM billing.prices
WHERE prices.merchant_id = sqlc.arg(merchant_id)::uuid AND EXISTS (SELECT 1 FROM billing.price_psp_bindings b WHERE b.merchant_id = sqlc.arg(merchant_id)::uuid AND b.price_id = billing.prices.id AND b.merchant_id = billing.prices.merchant_id AND b.psp_id = sqlc.arg(psp_id)::uuid);

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
-- PS-4: backfill a rail charge that has no local payment record.
-- Dedupe rides the payments_psp_id_transaction_id_key identity.
-- name: ReconcileBackfillPayment :execrows
INSERT INTO billing.payments (
    merchant_id, price_id, channel, rail, transaction_id, amount, list_amount, currency,
    status, subscription_id, metadata, purchased_at, customer_id, psp_id,
    money_movement
) VALUES (
    sqlc.arg(merchant_id)::uuid,
    sqlc.arg(price_id), 'rail', sqlc.arg(rail)::text,
    sqlc.arg(transaction_id), sqlc.arg(amount), sqlc.arg(amount),
    sqlc.arg(currency),
    'succeeded', sqlc.narg(subscription_id), sqlc.narg(metadata),
    COALESCE(NULLIF(sqlc.arg(purchased_at)::timestamptz, '0001-01-01 00:00:00+00'::timestamptz), now()),
    sqlc.arg(customer_id), sqlc.narg(psp_id)::uuid,
    -- or#827: the row mirrors a charge the rail actually settled.
    'rail'
)
ON CONFLICT DO NOTHING;

-- PS-5: record a rail refund that is missing locally as a negative-
-- amount payment row linked to the refunded payment. Same dedupe identity.
-- name: ReconcileRecordRefund :execrows
INSERT INTO billing.payments (
    merchant_id, price_id, channel, rail, transaction_id, amount, list_amount, currency,
    status, subscription_id, refunded_payment_id, metadata, purchased_at,
    customer_id, psp_id, reversal_kind, money_movement
) VALUES (
    sqlc.arg(merchant_id)::uuid,
    sqlc.arg(price_id), 'rail', sqlc.arg(rail)::text,
    sqlc.arg(transaction_id), sqlc.arg(amount), sqlc.arg(amount),
    sqlc.arg(currency),
    'succeeded', sqlc.narg(subscription_id), sqlc.narg(refunded_payment_id),
    sqlc.narg(metadata),
    COALESCE(NULLIF(sqlc.arg(purchased_at)::timestamptz, '0001-01-01 00:00:00+00'::timestamptz), now()),
    -- or#827: a refund is real (negative) money movement at the rail; the
    -- settlement feed excludes it on amount/refunded_payment_id, not on this.
    sqlc.arg(customer_id), sqlc.narg(psp_id)::uuid, 'refund', 'rail'
)
ON CONFLICT DO NOTHING;

-- name: ReconcileMarkPaymentRefunded :execrows
UPDATE billing.payments
SET status = 'refunded'
WHERE payments.merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id) AND status <> 'refunded' AND deleted_at IS NULL;

-- PS-1 materialization (bootstrap mode, --materialize): create the local
-- subscription for a rail subscription that resolved unambiguously to an
-- identity and a price. Its access follows the product like a normal signup. Idempotent: a second run inserts nothing
-- when any subscription already carries the rail subscription id (zero
-- rows returned = already materialized).
-- name: ReconcileMaterializeSubscription :many
INSERT INTO billing.subscriptions (
    merchant_id, price_id, product_id, status, rail, rail_subscription_id,
    current_period_starts_at, current_period_ends_at, started_at,
    access_duration_hours_snapshot, customer_id, psp_id, collection_policy, quantity
)
SELECT sqlc.arg(merchant_id)::uuid, pr.id, pr.product_id, sqlc.arg(status)::text,
       sqlc.arg(rail), NULLIF(sqlc.arg(rail_subscription_id)::text, ''),
       sqlc.narg(period_starts_at)::timestamptz,
       sqlc.narg(period_ends_at)::timestamptz,
       COALESCE(sqlc.narg(started_at)::timestamptz, now()),
       pr.access_duration_hours, sqlc.arg(customer_id), sqlc.arg(psp_id)::uuid, COALESCE(NULLIF(sqlc.arg(collection_policy)::text,''),'provider'),
       sqlc.arg(quantity)::int
FROM billing.prices pr
JOIN billing.products p ON p.id = pr.product_id
WHERE pr.merchant_id = sqlc.arg(merchant_id)::uuid AND p.merchant_id = sqlc.arg(merchant_id)::uuid AND pr.id = sqlc.arg(price_id)
  AND NOT EXISTS (
      SELECT 1 FROM billing.subscriptions s
      WHERE s.merchant_id = sqlc.arg(merchant_id)::uuid AND s.rail_subscription_id = sqlc.arg(rail_subscription_id)::text
        AND s.deleted_at IS NULL
        AND s.rail = ANY (sqlc.arg(rails)::text[])
        -- or#893: every writer resolves a PSP now, including the declared
        -- legacy-book import, so the dedupe is PSP-scoped like the reads. A
        -- provider subscription id is only unique within a gateway account.
        AND s.psp_id = sqlc.arg(psp_id)::uuid
  )
RETURNING id, product_id, access_duration_hours_snapshot;

-- PS-7: adopt the rail's vault metadata for a stored payment method.
-- name: ReconcileAdoptPaymentMethod :execrows
UPDATE billing.payment_methods
SET card_last4 = COALESCE(sqlc.narg(card_last4)::text, card_last4),
    card_exp_month = COALESCE(sqlc.narg(card_exp_month)::smallint, card_exp_month),
    card_exp_year = COALESCE(sqlc.narg(card_exp_year)::smallint, card_exp_year),
    updated_at = now()
WHERE id = sqlc.arg(id)
  AND merchant_id = sqlc.arg(merchant_id)
  AND ((sqlc.narg(card_last4)::text IS NOT NULL AND card_last4 IS DISTINCT FROM sqlc.narg(card_last4)::text)
       OR (sqlc.narg(card_exp_month)::smallint IS NOT NULL AND card_exp_month IS DISTINCT FROM sqlc.narg(card_exp_month)::smallint)
       OR (sqlc.narg(card_exp_year)::smallint IS NOT NULL AND card_exp_year IS DISTINCT FROM sqlc.narg(card_exp_year)::smallint));

-- #511 Convergence Engine: per-(merchant, source_domain) confirmed-absence gate.
-- WRITERS (#665): reconcile.MarkReconciledSourceDomains flips a domain
-- automatically after a pull PROVES it — exhaustive coverage
-- (SnapshotCoverage, not mere event-window watermark freshness) of EVERY
-- configured provider account whose rail could hold that domain's sources.
-- `grants` is admin/local-sourced, so no pull ever proves it — it stays a
-- manual/bulk-import decision. The flag is a ratchet: never auto-unset.

-- name: UpsertReconciliationState :one
-- Mark a source domain's reconciliation watermark: pass fully_reconciled=true
-- after a completed authoritative pull/import for that domain.
INSERT INTO billing.reconciliation_state (
    merchant_id, source_domain, fully_reconciled, updated_at
) VALUES (
    sqlc.arg(merchant_id)::uuid, sqlc.arg(source_domain)::text,
    sqlc.arg(fully_reconciled)::boolean, now()
)
ON CONFLICT (merchant_id, source_domain) DO UPDATE SET
    fully_reconciled = EXCLUDED.fully_reconciled,
    updated_at = now()
RETURNING *;

-- name: IsSourceDomainReconciled :one
-- The confirmed-absence gate (§3.2): is this source domain proven fully
-- reconciled for the merchant? Absent row = not yet reconciled = false.
SELECT COALESCE((
    SELECT fully_reconciled FROM billing.reconciliation_state
    WHERE merchant_id = sqlc.arg(merchant_id)::uuid
      AND source_domain = sqlc.arg(source_domain)::text
), false) AS fully_reconciled;

-- LIFE life.subscription.renewal_overdue (#1096): an active subscription a
-- provider bills whose paid period ended before overdue_before with no
-- renewal payment recorded. A clock reading only: the repair asks the
-- provider (RenewalOverdue -> unverified). Oldest lapse first, capped (or#837).
-- name: ListOverdueRenewals :many
SELECT s.id, s.rail, s.current_period_ends_at
FROM billing.subscriptions s
WHERE s.merchant_id = sqlc.arg(merchant_id)::uuid
  AND (sqlc.narg(customer_id)::uuid IS NULL OR s.customer_id = sqlc.narg(customer_id)::uuid)
  AND s.deleted_at IS NULL
  AND s.status = 'active'
  AND s.collection_policy <> 'engine'
  AND s.current_period_ends_at < sqlc.arg(overdue_before)::timestamptz
  AND NOT EXISTS (
      SELECT 1 FROM billing.payments p
      WHERE p.merchant_id = s.merchant_id AND p.subscription_id = s.id
        AND p.deleted_at IS NULL AND p.status = 'succeeded'
        AND p.purchased_at >= s.current_period_ends_at
  )
ORDER BY s.current_period_ends_at, s.id
LIMIT sqlc.arg(row_limit)::int;

-- LIFE life.subscription.grace_exhausted (#1096): a provider-billed
-- subscription past_due whose grace ended with no attempt scheduled. The
-- repair asks the provider (DunningStale -> unverified). Capped (or#837).
-- name: ListDunningPastGrace :many
SELECT s.id, s.current_period_ends_at, s.grace_ends_at
FROM billing.subscriptions s
WHERE s.merchant_id = sqlc.arg(merchant_id)::uuid
  AND (sqlc.narg(customer_id)::uuid IS NULL OR s.customer_id = sqlc.narg(customer_id)::uuid)
  AND s.deleted_at IS NULL
  AND s.status = 'past_due'
  AND s.collection_policy <> 'engine'
  AND s.next_retry_at IS NULL
  AND s.grace_ends_at < sqlc.arg(now)::timestamptz
ORDER BY s.grace_ends_at, s.id
LIMIT sqlc.arg(row_limit)::int;

-- #511 LIFE plane (life.subscription.pending_stale): pending subscriptions that
-- never confirmed within the threshold (cutoff = now - pendingStaleAfter).
-- name: ListStalePendingSubscriptions :many
SELECT s.id FROM billing.subscriptions s
WHERE s.merchant_id = sqlc.arg(merchant_id)::uuid
  AND (sqlc.narg(customer_id)::uuid IS NULL OR s.customer_id = sqlc.narg(customer_id)::uuid)
  AND s.deleted_at IS NULL
  AND s.status = 'pending'
  AND s.created_at < sqlc.arg(cutoff)::timestamptz
  AND NOT EXISTS (
      SELECT 1 FROM billing.payments p
      WHERE p.merchant_id = s.merchant_id AND p.subscription_id = s.id
        AND p.status = 'succeeded' AND p.deleted_at IS NULL
  )
-- or#837: oldest first, capped (see ListLapsedSubscriptionsWithEvidence).
ORDER BY s.created_at, s.id
LIMIT sqlc.arg(row_limit)::int;

-- name: ListPaidPendingSubscriptions :many
SELECT s.id, s.rail, s.created_at, p.id AS payment_id, p.transaction_id, p.purchased_at
FROM billing.subscriptions s
JOIN LATERAL (
    SELECT p.id, p.transaction_id, p.purchased_at FROM billing.payments p
    WHERE p.merchant_id = s.merchant_id AND p.subscription_id = s.id
      AND p.status = 'succeeded' AND p.deleted_at IS NULL
    ORDER BY p.purchased_at DESC, p.id DESC
    LIMIT 1
) p ON true
WHERE s.merchant_id = sqlc.arg(merchant_id)::uuid
  AND (sqlc.narg(customer_id)::uuid IS NULL OR s.customer_id = sqlc.narg(customer_id)::uuid)
  AND s.deleted_at IS NULL
  AND s.status = 'pending'
  AND s.created_at < sqlc.arg(cutoff)::timestamptz
ORDER BY s.created_at, s.id
LIMIT sqlc.arg(row_limit)::int;

-- #511 LIFE plane (life.provider_intent.abandoned): desired provider actions that
-- will not auto-retry (terminal/expired, or past their deadline) and need an
-- operator/admin. Surface-only (no auto-repair). Scoped by merchant (+ optional sub).
-- name: ListAbandonedProviderIntents :many
SELECT id, intent_type, status, rail FROM billing.provider_intents
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
SELECT id, rail, current_period_starts_at, current_period_ends_at, rail_subscription_id FROM billing.subscriptions
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND (sqlc.narg(customer_id)::uuid IS NULL OR customer_id = sqlc.narg(customer_id)::uuid)
  AND deleted_at IS NULL
  AND collection_policy <> 'engine'
  AND status = 'unverified'
  AND (sqlc.narg(rail)::text IS NULL OR rail = sqlc.narg(rail)::text)
ORDER BY current_period_ends_at ASC NULLS FIRST
LIMIT sqlc.arg(max_rows)::int;

-- LIFE plane (life.subscription.dunning_overdue): an OpenRails-dunned NMI
-- schedule past_due in grace with NO retry scheduled. Engine rows own their
-- schedule (past_due without a retry is their awaiting-new-card state);
-- provider-owned rows are retried by the provider or not at all.
-- name: ListDunningStalledSubscriptions :many
SELECT id, (COALESCE(retry_attempts, 0) >= 1 AND last_retry_at IS NOT NULL)::bool AS attempt_recorded
FROM billing.subscriptions
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND (sqlc.narg(customer_id)::uuid IS NULL OR customer_id = sqlc.narg(customer_id)::uuid)
  AND deleted_at IS NULL
  AND collection_policy = 'nmi_schedule'
  AND status = 'past_due'
  AND next_retry_at IS NULL
  AND (grace_ends_at IS NULL OR grace_ends_at > sqlc.arg(now)::timestamptz)
ORDER BY current_period_ends_at;

-- #665 DERIVE `derive.grant_effect.mismatch` (grant direction): an `active`
-- sub in a RUNNING period with a subscription grant but NO subscription window
-- (live OR revoked) of its product overlapping the period; a recorded revoke
-- is a recorded decision, never re-granted (spec §6). Excludes no-grant subs
-- (owned by derive.subscription.missing). Another source's overlapping access
-- does not satisfy this subscription. customer_id nullable: NULL = merchant-wide.
-- name: ListActiveSubsMissingAccessProjection :many
SELECT s.id, s.customer_id, s.product_id, s.status,
       s.current_period_starts_at, s.current_period_ends_at, s.started_at, s.ended_at, s.access_duration_hours_snapshot
FROM billing.subscriptions s
WHERE s.merchant_id = sqlc.arg(merchant_id)::uuid
  AND (sqlc.narg(customer_id)::uuid IS NULL OR s.customer_id = sqlc.narg(customer_id)::uuid)
  AND s.deleted_at IS NULL
  AND s.status = 'active'
  AND (s.access_duration_hours_snapshot IS NULL OR
       COALESCE(s.current_period_starts_at, s.started_at) + s.access_duration_hours_snapshot * interval '1 hour' > sqlc.arg(now)::timestamptz)
  AND COALESCE(s.current_period_starts_at, s.started_at) <= sqlc.arg(now)::timestamptz
  AND EXISTS (
      SELECT 1 FROM billing.grants g
      WHERE g.merchant_id = s.merchant_id AND g.event = 'grant'
        AND g.source_type = 'subscription' AND g.source_id = s.id::text
  )
  AND NOT EXISTS (
      SELECT 1 FROM billing.product_access pa
      WHERE pa.merchant_id = s.merchant_id
        AND pa.source_type = 'subscription' AND pa.source_id = s.id::text
        AND pa.product_id = s.product_id
        AND pa.deleted_at IS NULL
        AND (s.access_duration_hours_snapshot IS NULL OR
             pa.starts_at < COALESCE(s.current_period_starts_at, s.started_at) + s.access_duration_hours_snapshot * interval '1 hour')
        AND (pa.ends_at IS NULL OR pa.ends_at > COALESCE(s.current_period_starts_at, s.started_at))
  )
ORDER BY s.current_period_ends_at;

-- A chargeback revokes access. Ordinary cancellation only stops billing and
-- leaves all previously purchased access windows intact, including indefinite
-- and longer-than-billing-period terms.
-- name: ListDeadSubsWithLiveAccess :many
SELECT s.id, s.customer_id, s.status,
       NULL::timestamptz AS current_period_ends_at, s.canceled_at AS ended_at
FROM billing.subscriptions s
WHERE s.merchant_id = sqlc.arg(merchant_id)::uuid
  AND (sqlc.narg(customer_id)::uuid IS NULL OR s.customer_id = sqlc.narg(customer_id)::uuid)
  AND s.deleted_at IS NULL AND s.status = 'canceled' AND s.cancel_type = 'chargeback'
  AND EXISTS (
      SELECT 1 FROM billing.product_access e
      WHERE e.merchant_id = s.merchant_id
        AND e.source_type = 'subscription' AND e.source_id = s.id::text
        AND e.revoked_at IS NULL AND e.deleted_at IS NULL
        AND (e.ends_at IS NULL OR (e.ends_at > sqlc.arg(now)::timestamptz AND e.ends_at > s.canceled_at))
  )
ORDER BY s.canceled_at, s.id
LIMIT sqlc.arg(row_limit)::int;

-- #690 DERIVE `derive.access.unjustified` — the FREELOADER detector
-- (renamed from derive.entitlement.orphan in migration 066: "orphaned" is
-- reserved for the paying-without-access category). A LIVE
-- window (not revoked/deleted, started, unbounded or ending in the future)
-- whose justification chain is PROVEN broken. Post-#691 fail-open, "live
-- window past paid-through" is NORMAL for a standing auto-renew projection
-- (stale ≠ freeloader) — a freeloader's SOURCE is proven absent or reversed:
--   missing_subscription           - source_type=subscription, no sub row at all
--   refunded_payment               - purchase window whose payment was refunded,
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
-- Verification SQL (2026-07-01 host-one analysis, measured ZERO on the full
-- re-import): (1) grant-justification by source — live windows LEFT JOIN live
-- grants on (customer, source) counting NULLs per source_type; (2)
-- window-vs-paid-through by status — live windows joined to subscriptions
-- grouped by status comparing ends_at against GREATEST(current_period_ends_at,
-- ended_at). This query is the union of both, restricted to proven-dead
-- sources. customer_id nullable: NULL = merchant-wide sweep.
-- name: ListUnjustifiedAccessWindows :many
SELECT e.id AS access_id, e.customer_id, e.product_id,
       e.source_type, e.source_id, e.starts_at, e.ends_at,
       pay.id AS payment_id,
       pr.product_id AS payment_product_id,
       CASE
           WHEN e.source_type = 'subscription' AND s.id IS NULL THEN 'missing_subscription'
           ELSE 'refunded_payment'
       END::text AS cause
FROM billing.product_access e
LEFT JOIN billing.subscriptions s
       ON e.source_type = 'subscription' AND s.id::text = e.source_id AND s.merchant_id = e.merchant_id AND s.deleted_at IS NULL
LEFT JOIN billing.payments pay
       ON e.source_type = 'purchase' AND pay.id = e.payment_id AND pay.merchant_id = e.merchant_id AND pay.deleted_at IS NULL
LEFT JOIN billing.prices pr
       ON pr.id = pay.price_id AND pr.merchant_id = e.merchant_id
WHERE e.merchant_id = sqlc.arg(merchant_id)::uuid
  AND (sqlc.narg(customer_id)::uuid IS NULL OR e.customer_id = sqlc.narg(customer_id)::uuid)
  AND e.revoked_at IS NULL AND e.deleted_at IS NULL
  AND e.starts_at <= sqlc.arg(now)::timestamptz
  AND (e.ends_at IS NULL OR e.ends_at > sqlc.arg(now)::timestamptz)
  AND e.source_type IN ('subscription', 'purchase')
  -- no live un-terminated access grant covering now justifies the window
  AND NOT EXISTS (
      SELECT 1 FROM billing.grants g
      WHERE g.merchant_id = e.merchant_id
        AND g.customer_id = e.customer_id
        AND g.event = 'grant' AND g.kind = 'access'
        AND (g.id = e.grant_id OR (g.source_id = e.source_id AND g.source_type = e.source_type))
        AND g.starts_at <= sqlc.arg(now)::timestamptz
        AND (g.ends_at IS NULL OR g.ends_at > sqlc.arg(now)::timestamptz)
        AND NOT EXISTS (
            SELECT 1 FROM billing.grants t
            WHERE t.merchant_id = g.merchant_id AND t.supersedes_id = g.id
              AND t.event IN ('revoke', 'expire', 'supersede'))
  )
  -- a TERMINATED backing grant means derive.grant_effect.excess owns the retraction
  AND NOT EXISTS (
      SELECT 1 FROM billing.grants tg
      JOIN billing.grants t ON t.merchant_id = tg.merchant_id AND t.supersedes_id = tg.id
                             AND t.event IN ('revoke', 'expire', 'supersede')
      WHERE tg.merchant_id = e.merchant_id AND tg.id = e.grant_id
  )
  AND (
      (e.source_type = 'subscription' AND s.id IS NULL)
      OR (e.source_type = 'purchase' AND pay.id IS NOT NULL AND pay.status = 'refunded')
  )
-- or#837: oldest window first, capped. Surface-only findings, so truncation
-- delays an operator decision rather than losing one.
ORDER BY e.starts_at, e.id
LIMIT sqlc.arg(row_limit)::int;

-- #511 Phase E (Converge sweep worker): the no-GUC list of merchants
-- to sweep. merchants is a GLOBAL control-plane table.
-- name: ListActiveMerchantIDs :many
SELECT id FROM billing.merchants
WHERE status = 'active' AND deleted_at IS NULL
ORDER BY id;

-- #789 NOTIFY `notify.access_ended` detector: customers whose LAST access
-- window of a product closed inside (closed_after, now] — the close instant is
-- LEAST(ends_at, revoked_at) — with NO other live window of that product. One
-- row per customer (latest close): one email per customer, whatever ended the
-- access. customer_id nullable: NULL = merchant-wide sweep.
-- name: ListRecentlyClosedLastAccessWindows :many
SELECT DISTINCT ON (e.customer_id)
       e.id, e.customer_id, e.product_id,
       LEAST(COALESCE(e.ends_at, 'infinity'::timestamptz), COALESCE(e.revoked_at, 'infinity'::timestamptz)) AS closed_at,
       e.source_type, e.source_id
FROM billing.product_access e
WHERE e.merchant_id = sqlc.arg(merchant_id)::uuid
  AND (sqlc.narg(customer_id)::uuid IS NULL OR e.customer_id = sqlc.narg(customer_id)::uuid)
  AND e.deleted_at IS NULL
  AND (e.ends_at IS NOT NULL OR e.revoked_at IS NOT NULL)
  AND LEAST(COALESCE(e.ends_at, 'infinity'::timestamptz), COALESCE(e.revoked_at, 'infinity'::timestamptz)) > sqlc.arg(closed_after)::timestamptz
  AND LEAST(COALESCE(e.ends_at, 'infinity'::timestamptz), COALESCE(e.revoked_at, 'infinity'::timestamptz)) <= sqlc.arg(now)::timestamptz
  AND NOT EXISTS (
      SELECT 1 FROM billing.product_access live
      WHERE live.merchant_id = e.merchant_id
        AND live.customer_id = e.customer_id
        AND live.product_id = e.product_id
        AND live.deleted_at IS NULL AND live.revoked_at IS NULL
        AND live.starts_at <= sqlc.arg(now)::timestamptz
        AND (live.ends_at IS NULL OR live.ends_at > sqlc.arg(now)::timestamptz)
  )
  -- A tier change supersedes the replaced tier's window while the customer
  -- holds the new tier's access: that is not access ending.
  AND NOT (e.revoke_reason = 'superseded' AND EXISTS (
      SELECT 1 FROM billing.product_access nw
      WHERE nw.merchant_id = e.merchant_id
        AND nw.customer_id = e.customer_id
        AND nw.deleted_at IS NULL AND nw.revoked_at IS NULL
        AND (nw.ends_at IS NULL OR nw.ends_at > sqlc.arg(now)::timestamptz)
  ))
ORDER BY e.customer_id,
         LEAST(COALESCE(e.ends_at, 'infinity'::timestamptz), COALESCE(e.revoked_at, 'infinity'::timestamptz)) DESC;

-- name: ResolveStandingFinding :execrows
-- A standing finding whose subject is healthy again closes itself.
UPDATE billing.reconciliation_findings
   SET status = 'fixed', resolution = 'auto_vanished', resolved_at = now()
 WHERE merchant_id = sqlc.arg(merchant_id)::uuid
   AND finding_type = sqlc.arg(finding_type)::text
   AND subject_key = sqlc.arg(subject_key)::text
   AND status IN ('reconcile_required', 'requires_review');

-- name: ResolveClearedFindings :execrows
-- Open findings of the given standing types that the latest merchant-wide
-- converge no longer reports (keep = type || chr(31) || subject) have cleared.
UPDATE billing.reconciliation_findings
   SET status = 'fixed', resolution = 'auto_vanished', resolved_at = now(),
       notified_at = NULL, notified_severity = NULL, updated_at = now()
 WHERE merchant_id = sqlc.arg(merchant_id)::uuid
   AND finding_type = ANY(sqlc.arg(finding_types)::text[])
   AND status IN ('reconcile_required', 'requires_review')
   AND NOT ((finding_type || chr(31) || subject_key) = ANY(sqlc.arg(keep)::text[]));

-- name: SummarizeHeldEngineRenewals :one
-- LIFE life.renewal.held: engine renewals with no outcome past their allowance,
-- min(24h, max(5m, period/10)) after the paid period. Collection is stopped.
SELECT count(*)::int AS held,
       COALESCE(min(s.current_period_ends_at), sqlc.arg(now)::timestamptz)::timestamptz AS oldest_due_at
FROM billing.subscriptions s
WHERE s.merchant_id = sqlc.arg(merchant_id)::uuid
  AND s.deleted_at IS NULL AND s.canceled_at IS NULL
  AND s.status = 'active' AND s.collection_policy = 'engine'
  AND s.current_period_ends_at > s.current_period_starts_at
  AND s.current_period_ends_at + LEAST(interval '24 hours', GREATEST(interval '5 minutes',
      (s.current_period_ends_at - s.current_period_starts_at) / 10)) <= sqlc.arg(now)::timestamptz;

-- LIFE life.unverified.* (#1094/#1096): unverified subscriptions with the
-- instant they became unverified, oldest first, capped (or#837).
-- name: ListUnverifiedSubscriptions :many
SELECT s.id, s.psp_id, s.rail,
       COALESCE(v.unverified_at, s.updated_at)::timestamptz AS unverified_at,
       COALESCE(v.reads, 0)::int AS reads, v.last_read_at
FROM billing.subscriptions s
LEFT JOIN billing.subscription_verifications v ON v.merchant_id = s.merchant_id AND v.subscription_id = s.id
WHERE s.merchant_id = sqlc.arg(merchant_id)::uuid
  AND (sqlc.narg(customer_id)::uuid IS NULL OR s.customer_id = sqlc.narg(customer_id)::uuid)
  AND s.deleted_at IS NULL
  AND s.status = 'unverified'
ORDER BY 4, s.id
LIMIT sqlc.arg(row_limit)::int;

-- LIFE life.dunning.funnel (#1096): live subscriptions by lifecycle state and
-- the age of the oldest unverified entry (0 when none).
-- name: CountSubscriptionFunnel :one
SELECT count(*) FILTER (WHERE s.status = 'active')::bigint AS active,
       count(*) FILTER (WHERE s.status = 'past_due')::bigint AS past_due,
       count(*) FILTER (WHERE s.status = 'awaiting_method')::bigint AS awaiting_method,
       count(*) FILTER (WHERE s.status = 'unverified')::bigint AS unverified,
       COALESCE(EXTRACT(EPOCH FROM (sqlc.arg(now)::timestamptz - min(COALESCE(v.unverified_at, s.updated_at)) FILTER (WHERE s.status = 'unverified'))), 0)::bigint AS oldest_unverified_age_seconds
FROM billing.subscriptions s
LEFT JOIN billing.subscription_verifications v ON v.merchant_id = s.merchant_id AND v.subscription_id = s.id
WHERE s.merchant_id = sqlc.arg(merchant_id)::uuid
  AND s.deleted_at IS NULL
  AND s.status IN ('active', 'past_due', 'awaiting_method', 'unverified');

-- life.dunning.funnel: provider operations whose outcome is unknown (a charge
-- that may or may not have happened), with the oldest one's age.
-- name: CountUnknownOperations :one
SELECT count(*)::bigint AS open_count,
       COALESCE(EXTRACT(EPOCH FROM (sqlc.arg(now)::timestamptz - min(created_at))), 0)::bigint AS oldest_age_seconds
FROM billing.provider_intents
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND status = 'unknown_needs_verify';

-- A LIFE repair's premise, re-checked under the row lock: a completed payment
-- for the subscription (at or after since, when given).
-- name: SubscriptionHasCompletedPayment :one
SELECT EXISTS (
    SELECT 1 FROM billing.payments p
    WHERE p.merchant_id = sqlc.arg(merchant_id)::uuid
      AND p.subscription_id = sqlc.arg(subscription_id)::uuid
      AND p.deleted_at IS NULL AND p.status = 'succeeded'
      AND (sqlc.narg(since)::timestamptz IS NULL OR p.purchased_at >= sqlc.narg(since)::timestamptz)
)::bool AS paid;

-- name: ListUnverifiedSubscriptionIDsIn :many
SELECT id FROM billing.subscriptions
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND id = ANY(sqlc.arg(ids)::uuid[])
  AND status = 'unverified' AND deleted_at IS NULL
  AND collection_policy <> 'engine' AND rail_subscription_id IS NOT NULL;

-- Callers pass a limit one above their bulk threshold.
-- name: ListUnverifiedNMISubscriptionIDs :many
SELECT id FROM billing.subscriptions
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND rail = 'nmi' AND status = 'unverified' AND deleted_at IS NULL
  AND collection_policy <> 'engine' AND rail_subscription_id IS NOT NULL
ORDER BY current_period_ends_at NULLS FIRST
LIMIT sqlc.arg(row_limit)::bigint;

-- name: ListUnverifiedSubscriptionIDsForPSP :many
SELECT id FROM billing.subscriptions
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND psp_id = sqlc.arg(psp_id)::uuid
  AND status = 'unverified' AND deleted_at IS NULL
  AND collection_policy <> 'engine' AND rail_subscription_id IS NOT NULL
ORDER BY current_period_ends_at NULLS FIRST;

-- name: RecordSubscriptionVerificationReads :exec
UPDATE billing.subscription_verifications
SET reads = reads + 1, last_read_at = sqlc.arg(read_at)::timestamptz, last_error = sqlc.narg(last_error)::text
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND subscription_id = ANY(sqlc.arg(subscription_ids)::uuid[]);

-- name: ListSubscriptionVaultRefs :many
SELECT s.id, pm.rail_customer_ref
FROM billing.subscriptions s
JOIN billing.payment_methods pm ON pm.merchant_id = s.merchant_id AND pm.id = s.payment_method_id
WHERE s.merchant_id = sqlc.arg(merchant_id)::uuid AND s.id = ANY(sqlc.arg(ids)::uuid[]) AND s.deleted_at IS NULL;

-- Recorded charges (payments) and declines (attempts) a bulk pass decides from.
-- name: ListRecordedSubscriptionCharges :many
SELECT subscription_id, transaction_id::text AS transaction_id, 'succeeded'::text AS status,
       purchased_at::timestamptz AS occurred_at, amount::bigint AS amount, currency::text AS currency,
       ''::text AS response_code
FROM billing.payments
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND subscription_id = ANY(sqlc.arg(subscription_ids)::uuid[])
  AND purchased_at >= sqlc.arg(since)::timestamptz AND status = 'succeeded' AND deleted_at IS NULL
UNION ALL
SELECT subscription_id, transaction_id::text, 'failed'::text, attempted_at::timestamptz, amount::bigint,
       COALESCE(currency, '')::text, COALESCE(response_code, '')::text
FROM billing.payment_attempts
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND subscription_id = ANY(sqlc.arg(subscription_ids)::uuid[])
  AND attempted_at >= sqlc.arg(since)::timestamptz AND category <> 'approved' AND transaction_id IS NOT NULL;

-- Resumes an interrupted bulk read, or starts one.
-- name: StartNMIBulkCheckpoint :one
INSERT INTO billing.nmi_bulk_checkpoints (merchant_id, psp_id, window_starts_at, window_ends_at, next_page, started_at)
VALUES (sqlc.arg(merchant_id)::uuid, sqlc.arg(psp_id)::uuid, sqlc.arg(window_starts_at)::timestamptz,
        sqlc.arg(window_ends_at)::timestamptz, 1, sqlc.arg(window_ends_at)::timestamptz)
ON CONFLICT (merchant_id, psp_id) DO UPDATE SET merchant_id = EXCLUDED.merchant_id
RETURNING window_starts_at, window_ends_at, next_page;

-- name: SetNMIBulkCheckpointPage :exec
UPDATE billing.nmi_bulk_checkpoints SET next_page = sqlc.arg(next_page)::bigint
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND psp_id = sqlc.arg(psp_id)::uuid;

-- name: DeleteNMIBulkCheckpoint :exec
DELETE FROM billing.nmi_bulk_checkpoints
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND psp_id = sqlc.arg(psp_id)::uuid;

-- name: AutoResolveFindingBySubject :exec
UPDATE billing.reconciliation_findings
SET status = 'fixed', resolution = 'auto_vanished', resolved_at = now(),
    notified_at = NULL, notified_severity = NULL, updated_at = now()
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND finding_type = sqlc.arg(finding_type)::text
  AND subject_key = sqlc.arg(subject_key)::text
  AND status IN ('reconcile_required', 'requires_review');

-- name: AutoResolveReviewFindingsByType :exec
UPDATE billing.reconciliation_findings
SET status = 'fixed', resolution = 'auto_vanished', resolved_at = now(),
    notified_at = NULL, notified_severity = NULL, updated_at = now()
WHERE merchant_id = sqlc.arg(merchant_id)::uuid AND finding_type = ANY(sqlc.arg(finding_types)::text[])
  AND status = 'requires_review';

-- Canonical invoice receipts live outside billing.payments. Recognize settled
-- NMI charges by their exact accepted PSP and transaction, never by a vault.
-- name: ReconcileListInvoicePaymentsByTransactionIDs :many
SELECT id, customer_id, invoice_id, rail_payment_id::text, amount, currency
FROM billing.invoice_payments
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND psp_id = sqlc.arg(psp_id)::uuid AND rail = sqlc.arg(rail)::text AND status = 'settled'
  AND rail_payment_id IS NOT NULL
  AND rail_payment_id = ANY(sqlc.arg(transaction_ids)::text[]);
