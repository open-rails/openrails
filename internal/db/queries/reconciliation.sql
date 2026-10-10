-- Reconciliation runs and findings, the engine's local-state reads, and the
-- enforce appliers' idempotent local writes. Statements that read
-- billing.current_merchant_id() need a merchant-pinned connection.

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
    -- An inline auto_fixed is a resolution: clear the notify linkage so a
    -- reopen of this identity notifies again.
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
-- Dedupe linkage for the immediate notify path: set once a finding notifies,
-- cleared by every resolution so a reopened finding notifies again.
UPDATE billing.reconciliation_findings
SET notified_at = sqlc.arg(notified_at)::timestamptz,
    notified_severity = sqlc.arg(severity)::text
WHERE reconciliation_findings.merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id);

-- By-id admin read. The resolve acts on whatever the finding names, so the
-- merchant predicate (the pinned connection's merchant) is its only scope.
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
-- covering their PSP vanished on their own. Batched by row_limit.
-- name: AutoResolveVanishedReconciliationFindings :execrows
UPDATE billing.reconciliation_findings
SET status = 'fixed',
    resolution = 'auto_vanished',
    resolved_at = now(),
    notified_at = NULL, notified_severity = NULL, -- resolution clears the notify linkage
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
    notified_at = NULL, notified_severity = NULL, -- resolution clears the notify linkage
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
    notified_at = NULL, notified_severity = NULL, -- resolution clears the notify linkage
    updated_at = now()
WHERE reconciliation_findings.merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id) AND status IN ('reconcile_required', 'requires_review', 'ignored');

-- name: MarkReconciliationFindingAutoFixed :execrows
UPDATE billing.reconciliation_findings
SET status = 'auto_fixed',
    resolution = 'enforced',
    evidence = jsonb_set(COALESCE(evidence, '{}'::jsonb), '{resolution}', sqlc.narg(resolution_evidence)::jsonb, true),
    resolved_at = now(),
    notified_at = NULL, notified_severity = NULL, -- resolution clears the notify linkage
    updated_at = now()
WHERE reconciliation_findings.merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id) AND status IN ('reconcile_required', 'requires_review');

-- name: AckReconciliationFinding :execrows
UPDATE billing.reconciliation_findings
SET status = 'fixed',
    resolution = 'admin_fixed',
    operator_notes = sqlc.narg(operator_notes),
    resolved_at = now(),
    notified_at = NULL, notified_severity = NULL, -- resolution clears the notify linkage
    updated_at = now()
WHERE reconciliation_findings.merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id) AND status IN ('reconcile_required', 'requires_review', 'auto_fixed');

-- name: DismissReconciliationFinding :execrows
UPDATE billing.reconciliation_findings
SET status = 'ignored',
    resolution = 'ignored',
    operator_notes = sqlc.narg(operator_notes),
    resolved_at = now(),
    notified_at = NULL, notified_severity = NULL, -- resolution clears the notify linkage
    updated_at = now()
WHERE reconciliation_findings.merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id) AND status IN ('reconcile_required', 'requires_review', 'auto_fixed', 'fixed');

-- The operator work list: open findings unless a status filter is given,
-- critical first, then oldest first; keyset-paged.
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
    notified_at = NULL, notified_severity = NULL, -- resolution clears the notify linkage
    updated_at = now()
WHERE id = sqlc.arg(id)
  AND merchant_id = billing.current_merchant_id() -- defence in depth, see GetReconciliationFinding
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
    notified_at = NULL, notified_severity = NULL, -- resolution clears the notify linkage
    updated_at = now()
WHERE id = sqlc.arg(id)
  AND merchant_id = billing.current_merchant_id() -- defence in depth, see GetReconciliationFinding
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
  AND merchant_id = billing.current_merchant_id() -- defence in depth, see GetReconciliationFinding
  AND status IN ('reconcile_required', 'requires_review');

-- name: ReconcileListSubscriptionsByRails :many
SELECT subscriptions.id, subscriptions.customer_id, subscriptions.price_id, subscriptions.product_id,
       subscriptions.status, subscriptions.rail, subscriptions.collection_policy, subscriptions.rail_subscription_id,
       charged.id AS payment_method_id,
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
LEFT JOIN billing.payment_methods charged ON charged.merchant_id = subscriptions.merchant_id
  AND charged.id = billing.subscription_payment_method_id(subscriptions.merchant_id, subscriptions.customer_id, subscriptions.payment_method_id, subscriptions.price_id, subscriptions.rail, subscriptions.collection_policy)
WHERE subscriptions.merchant_id = sqlc.arg(merchant_id)::uuid AND subscriptions.rail = ANY (sqlc.arg(rails)::text[])
  AND subscriptions.deleted_at IS NULL
  AND subscriptions.psp_id = sqlc.arg(psp_id)::uuid;

-- name: ReconcileListPaymentsByTransactionIDs :many
SELECT id, customer_id, rail, transaction_id, amount, currency, status,
       subscription_id, refunded_payment_id, purchased_at, invoice_id
FROM billing.payments
WHERE payments.merchant_id = sqlc.arg(merchant_id)::uuid AND rail::text = ANY (sqlc.arg(rails)::text[])
  AND deleted_at IS NULL
  AND transaction_id = ANY (sqlc.arg(transaction_ids)::text[])
  AND psp_id = sqlc.arg(psp_id)::uuid;

-- name: ReconcileListPaymentMethodsByRails :many
-- rail_customer_ref is the rail's handle on the stored instrument (on NMI the
-- customer_vault_id).
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

-- Prices bound to this PSP (price_psp_bindings), archived included:
-- grandfathered subscriptions still bill them.
-- name: ReconcileListPricesWithPSPLinks :many
SELECT id, product_id, amount, currency, access_duration_hours, billing_interval_hours, archived
FROM billing.prices
WHERE prices.merchant_id = sqlc.arg(merchant_id)::uuid AND EXISTS (SELECT 1 FROM billing.price_psp_bindings b WHERE b.merchant_id = sqlc.arg(merchant_id)::uuid AND b.price_id = billing.prices.id AND b.merchant_id = billing.prices.merchant_id AND b.psp_id = sqlc.arg(psp_id)::uuid);

-- Enforce appliers: idempotent local writes only, never a provider call.
-- Subscription state transitions go through reconcile.Decide and
-- reconcile.ApplyDecision instead.

-- Backfills a rail charge that has no local payment record, deduped on
-- payments_psp_id_transaction_id_key.
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
    -- The row mirrors a charge the rail actually settled.
    'rail'
)
ON CONFLICT DO NOTHING;

-- Records a rail refund missing locally as a negative-amount payment row linked
-- to the refunded payment. Same dedupe identity.
-- name: ReconcileRecordRefund :execrows
INSERT INTO billing.payments (
    merchant_id, price_id, channel, rail, transaction_id, amount, list_amount, currency,
    status, subscription_id, refunded_payment_id, metadata, purchased_at,
    customer_id, psp_id, reversal_kind, money_movement, order_id, invoice_id
) VALUES (
    sqlc.arg(merchant_id)::uuid,
    sqlc.narg(price_id), 'rail', sqlc.arg(rail)::text,
    sqlc.arg(transaction_id), sqlc.arg(amount), sqlc.arg(amount),
    sqlc.arg(currency),
    'succeeded', sqlc.narg(subscription_id), sqlc.narg(refunded_payment_id),
    sqlc.narg(metadata),
    COALESCE(NULLIF(sqlc.arg(purchased_at)::timestamptz, '0001-01-01 00:00:00+00'::timestamptz), now()),
    -- A refund is real (negative) money movement at the rail; the settlement
    -- feed excludes it on amount/refunded_payment_id, not on this.
    sqlc.arg(customer_id), sqlc.narg(psp_id)::uuid, 'refund', 'rail',
    -- A refund names what its charge paid.
    (SELECT o.order_id FROM billing.payments o WHERE o.merchant_id = sqlc.arg(merchant_id)::uuid AND o.id = sqlc.narg(refunded_payment_id)::uuid),
    (SELECT o.invoice_id FROM billing.payments o WHERE o.merchant_id = sqlc.arg(merchant_id)::uuid AND o.id = sqlc.narg(refunded_payment_id)::uuid)
)
ON CONFLICT DO NOTHING;

-- name: ReconcileMarkPaymentRefunded :execrows
UPDATE billing.payments
SET status = 'refunded'
WHERE payments.merchant_id = sqlc.arg(merchant_id)::uuid AND id = sqlc.arg(id) AND status <> 'refunded' AND deleted_at IS NULL;

-- Materialization (--materialize): creates the local subscription for a rail
-- subscription resolved unambiguously to an identity and a price; access
-- follows the product as on signup. Zero rows = already materialized.
-- name: ReconcileMaterializeSubscription :many
INSERT INTO billing.subscriptions (
    merchant_id, price_id, product_id, status, rail, rail_subscription_id,
    current_period_starts_at, current_period_ends_at, started_at,
    access_duration_hours_snapshot, customer_id, psp_id, collection_policy
)
SELECT sqlc.arg(merchant_id)::uuid, pr.id, pr.product_id, sqlc.arg(status)::text,
       sqlc.arg(rail), NULLIF(sqlc.arg(rail_subscription_id)::text, ''),
       sqlc.narg(period_starts_at)::timestamptz,
       sqlc.narg(period_ends_at)::timestamptz,
       COALESCE(sqlc.narg(started_at)::timestamptz, now()),
       pr.access_duration_hours, sqlc.arg(customer_id), sqlc.arg(psp_id)::uuid, COALESCE(NULLIF(sqlc.arg(collection_policy)::text,''),'provider')
FROM billing.prices pr
JOIN billing.products p ON p.id = pr.product_id
WHERE pr.merchant_id = sqlc.arg(merchant_id)::uuid AND p.merchant_id = sqlc.arg(merchant_id)::uuid AND pr.id = sqlc.arg(price_id)
  AND NOT EXISTS (
      SELECT 1 FROM billing.subscriptions s
      WHERE s.merchant_id = sqlc.arg(merchant_id)::uuid AND s.rail_subscription_id = sqlc.arg(rail_subscription_id)::text
        AND s.deleted_at IS NULL
        AND s.rail = ANY (sqlc.arg(rails)::text[])
        -- A provider subscription id is unique only within a gateway account,
        -- so the dedupe is PSP-scoped.
        AND s.psp_id = sqlc.arg(psp_id)::uuid
  )
RETURNING id, product_id, access_duration_hours_snapshot;

-- Adopts the rail's vault metadata for a stored payment method.
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

-- The per-(merchant, source_domain) confirmed-absence gate.
-- reconcile.MarkReconciledSourceDomains sets it once a pull proves exhaustive
-- coverage of every PSP whose rail could hold the domain's sources. No pull
-- proves the local-sourced grants domain; it stays a manual decision. Pulls
-- never unset the flag.

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
-- The confirmed-absence gate: is this source domain proven fully reconciled for
-- the merchant? No row = false.
SELECT COALESCE((
    SELECT fully_reconciled FROM billing.reconciliation_state
    WHERE merchant_id = sqlc.arg(merchant_id)::uuid
      AND source_domain = sqlc.arg(source_domain)::text
), false) AS fully_reconciled;

-- LIFE life.subscription.renewal_overdue: an active provider-billed subscription
-- whose paid period ended before overdue_before with no renewal payment. A clock
-- reading only: the repair asks the provider (RenewalOverdue -> unverified).
-- Oldest lapse first, capped.
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

-- LIFE life.subscription.grace_exhausted: a provider-billed subscription
-- past_due whose grace ended with no attempt scheduled. The repair asks the
-- provider (DunningStale -> unverified). Capped.
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

-- LIFE life.subscription.pending_stale: pending subscriptions unconfirmed past
-- the threshold (cutoff = now - pendingStaleAfter).
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
-- Oldest first, capped.
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

-- LIFE life.provider_intent.abandoned: provider actions that will not auto-retry
-- (terminal, expired, or past their deadline). Surface-only; optional
-- subscription filter.
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

-- The unverified cohort awaiting provider verification, oldest period first,
-- bounded per call so a provider pull windows it in batches. NULLS FIRST: rows
-- without a local period (legacy imports) resolve only here and must not starve
-- behind the dated cohort.
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

-- derive.grant_effect.mismatch (grant direction): an active subscription in a
-- running period with a subscription grant but no subscription window (live or
-- revoked) of its product overlapping the period; a recorded revoke is never
-- re-granted. Subscriptions without a grant are derive.subscription.missing's.
-- NULL customer_id = merchant-wide.
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

-- derive.access.unjustified (freeloader): a live window whose source is proven
-- absent or reversed: missing_subscription or refunded_payment. Never fires while
-- a live access grant covers now (a live window past paid-through is normal for
-- auto-renew), nor when the backing grant is terminated
-- (derive.grant_effect.excess's). Surface-only: revoking access is an operator
-- decision. NULL customer_id = merchant-wide.
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
-- Oldest window first, capped: truncation delays an operator decision rather
-- than losing one.
ORDER BY e.starts_at, e.id
LIMIT sqlc.arg(row_limit)::int;

-- CROSS-MERCHANT: the active merchants the converge sweep walks.
-- name: ListActiveMerchantIDs :many
SELECT id FROM billing.merchants
WHERE status = 'active' AND deleted_at IS NULL
ORDER BY id;

-- notify.access_ended: customers whose last window of a product closed
-- (LEAST(ends_at, revoked_at)) in (closed_after, now] with no other live window
-- of it. One row per customer (latest close): one email, whatever ended the
-- access. NULL customer_id = merchant-wide.
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

-- LIFE life.unverified.*: unverified subscriptions with the instant they became
-- unverified, oldest first, capped.
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

-- LIFE life.dunning.funnel: live subscriptions by lifecycle state and the age of
-- the oldest unverified entry (0 when none).
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
JOIN billing.payment_methods pm ON pm.merchant_id = s.merchant_id AND pm.id = billing.subscription_payment_method_id(s.merchant_id, s.customer_id, s.payment_method_id, s.price_id, s.rail, s.collection_policy)
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

