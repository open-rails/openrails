-- Retention sweep: due-work discovery.

-- CROSS-MERCHANT: merchants holding at least one row past a retention cutoff.
-- Ids only; every delete runs per merchant in bounded batches. Capped: a job
-- is bounded work and the next continues after the last merchant handled.
-- name: ListRetentionWorkMerchants :many
SELECT q.mid AS merchant_id
FROM (
    (SELECT DISTINCT we.merchant_id AS mid
       FROM billing.webhook_events we
      WHERE (sqlc.narg(after)::uuid IS NULL OR we.merchant_id > sqlc.narg(after)::uuid)
        AND we.completed_at < sqlc.arg(webhook_cutoff)::timestamptz
      ORDER BY 1 LIMIT sqlc.arg(merchant_limit)::int)
    UNION
    (SELECT DISTINCT ho.merchant_id AS mid
       FROM billing.host_outbox ho
      WHERE (sqlc.narg(after)::uuid IS NULL OR ho.merchant_id > sqlc.narg(after)::uuid)
        AND ho.recorded_at < sqlc.arg(outbox_cutoff)::timestamptz
      ORDER BY 1 LIMIT sqlc.arg(merchant_limit)::int)
    UNION
    (SELECT DISTINCT pa.merchant_id AS mid
       FROM billing.payment_attempts pa
      WHERE (sqlc.narg(after)::uuid IS NULL OR pa.merchant_id > sqlc.narg(after)::uuid)
        AND pa.attempted_at < sqlc.arg(attempt_cutoff)::timestamptz
      ORDER BY 1 LIMIT sqlc.arg(merchant_limit)::int)
    UNION
    (SELECT DISTINCT rc.merchant_id AS mid
       FROM billing.rebill_cycles rc
      WHERE (sqlc.narg(after)::uuid IS NULL OR rc.merchant_id > sqlc.narg(after)::uuid)
        AND rc.due_at < sqlc.arg(attempt_cutoff)::timestamptz
      ORDER BY 1 LIMIT sqlc.arg(merchant_limit)::int)
    UNION
    (SELECT DISTINCT nh.merchant_id AS mid
       FROM billing.nmi_history_months nh
      WHERE (sqlc.narg(after)::uuid IS NULL OR nh.merchant_id > sqlc.narg(after)::uuid)
        AND nh.month_at < sqlc.arg(attempt_cutoff)::timestamptz
      ORDER BY 1 LIMIT sqlc.arg(merchant_limit)::int)
    UNION
    (SELECT DISTINCT st.merchant_id AS mid
       FROM billing.subscription_status_transitions st
      WHERE (sqlc.narg(after)::uuid IS NULL OR st.merchant_id > sqlc.narg(after)::uuid)
        AND st.occurred_at < sqlc.arg(transition_cutoff)::timestamptz
      ORDER BY 1 LIMIT sqlc.arg(merchant_limit)::int)
    UNION
    (SELECT DISTINCT st.merchant_id AS mid FROM billing.subscription_billing_changes st
      WHERE (sqlc.narg(after)::uuid IS NULL OR st.merchant_id > sqlc.narg(after)::uuid)
        AND st.occurred_at < sqlc.arg(transition_cutoff)::timestamptz
      ORDER BY 1 LIMIT sqlc.arg(merchant_limit)::int)
    UNION
    (SELECT DISTINCT ca.merchant_id AS mid
       FROM billing.invoice_attempts ca
      WHERE (sqlc.narg(after)::uuid IS NULL OR ca.merchant_id > sqlc.narg(after)::uuid)
        AND ca.status = 'expired' AND ca.deleted_at IS NULL
        AND ca.payment_id IS NULL AND ca.subscription_id IS NULL AND ca.transaction_id IS NULL
        AND ca.expires_at < sqlc.arg(invoice_attempt_cutoff)::timestamptz
        AND NOT EXISTS (SELECT 1 FROM billing.solana_pay_references r WHERE r.merchant_id = ca.merchant_id AND r.invoice_attempt_id = ca.id)
        AND NOT EXISTS (SELECT 1 FROM billing.solana_pay_receipts rc WHERE rc.merchant_id = ca.merchant_id AND rc.invoice_attempt_id = ca.id)
        AND NOT EXISTS (SELECT 1 FROM billing.provider_intents pi WHERE pi.merchant_id = ca.merchant_id AND pi.payload ? 'order_attempt_id' AND pi.payload->>'order_attempt_id' = ca.id::text)
      ORDER BY 1 LIMIT sqlc.arg(merchant_limit)::int)
    UNION
    (SELECT DISTINCT o.merchant_id AS mid
       FROM billing.invoices o
      WHERE (sqlc.narg(after)::uuid IS NULL OR o.merchant_id > sqlc.narg(after)::uuid)
        AND o.collection_owner = 'engine' AND o.status IN ('canceled', 'expired') AND o.payment_id IS NULL AND o.attempt_id IS NULL
        AND COALESCE(o.canceled_at, o.expired_at) < sqlc.arg(unpaid_invoice_cutoff)::timestamptz
      ORDER BY 1 LIMIT sqlc.arg(merchant_limit)::int)
    UNION
    (SELECT DISTINCT wh.merchant_id AS mid
       FROM billing.webhook_health_daily wh
      WHERE (sqlc.narg(after)::uuid IS NULL OR wh.merchant_id > sqlc.narg(after)::uuid)
        AND wh.day_at < sqlc.arg(counter_cutoff)::timestamptz
      ORDER BY 1 LIMIT sqlc.arg(merchant_limit)::int)
    UNION
    (SELECT DISTINCT rf.merchant_id AS mid
       FROM billing.reconciliation_findings rf
      WHERE (sqlc.narg(after)::uuid IS NULL OR rf.merchant_id > sqlc.narg(after)::uuid)
        AND rf.resolved_at IS NOT NULL
        AND GREATEST(rf.resolved_at, rf.last_seen_at) < sqlc.arg(finding_cutoff)::timestamptz
      ORDER BY 1 LIMIT sqlc.arg(merchant_limit)::int)
    UNION
    (SELECT DISTINCT pi.merchant_id AS mid
       FROM billing.provider_intents pi
      WHERE (sqlc.narg(after)::uuid IS NULL OR pi.merchant_id > sqlc.narg(after)::uuid)
        AND pi.status IN ('succeeded', 'failed_terminal', 'superseded', 'expired')
        AND pi.destructive_run_id IS NULL
        AND pi.intent_type IN ('nmi_delete_subscription', 'stripe_cancel_subscription', 'stripe_resume_subscription', 'ccbill_cancel_subscription', 'nmi_payment_method_update', 'nmi_payment_source_update', 'nmi_card_vault', 'network_token', 'stripe_archive_price', 'stripe_archive_product', 'solana_sunset_plan', 'account_updater')
        AND pi.updated_at < sqlc.arg(provider_write_cutoff)::timestamptz
      ORDER BY 1 LIMIT sqlc.arg(merchant_limit)::int)
    UNION
    (SELECT DISTINCT ol.merchant_id AS mid
       FROM billing.operation_log ol
      WHERE (sqlc.narg(after)::uuid IS NULL OR ol.merchant_id > sqlc.narg(after)::uuid)
        AND ol.occurred_at < sqlc.arg(provider_write_cutoff)::timestamptz
      ORDER BY 1 LIMIT sqlc.arg(merchant_limit)::int)
    UNION
    (SELECT DISTINCT pk.merchant_id AS mid
       FROM billing.parked_provider_subscriptions pk
      WHERE (sqlc.narg(after)::uuid IS NULL OR pk.merchant_id > sqlc.narg(after)::uuid)
        AND pk.released_at < sqlc.arg(park_cutoff)::timestamptz
      ORDER BY 1 LIMIT sqlc.arg(merchant_limit)::int)
    UNION
    (SELECT DISTINCT lw.merchant_id AS mid
       FROM billing.limit_windows lw
      WHERE (sqlc.narg(after)::uuid IS NULL OR lw.merchant_id > sqlc.narg(after)::uuid)
        AND lw.window_ends_at < sqlc.arg(limit_window_cutoff)::timestamptz
        AND (lw.reached_at IS NULL OR lw.window_ends_at < sqlc.arg(reached_limit_window_cutoff)::timestamptz)
      ORDER BY 1 LIMIT sqlc.arg(merchant_limit)::int)
    UNION
    (SELECT DISTINCT le.merchant_id AS mid
       FROM billing.leases le
      WHERE (sqlc.narg(after)::uuid IS NULL OR le.merchant_id > sqlc.narg(after)::uuid)
        AND le.last_sync_at < sqlc.arg(replay_cutoff)::timestamptz
      ORDER BY 1 LIMIT sqlc.arg(merchant_limit)::int)
    UNION
    (SELECT DISTINCT la.merchant_id AS mid
       FROM billing.limit_adjustments la
      WHERE (sqlc.narg(after)::uuid IS NULL OR la.merchant_id > sqlc.narg(after)::uuid)
        AND la.created_at < sqlc.arg(replay_cutoff)::timestamptz
      ORDER BY 1 LIMIT sqlc.arg(merchant_limit)::int)
    UNION
    (SELECT DISTINCT cm.merchant_id AS mid
       FROM billing.customer_messages cm
      WHERE (sqlc.narg(after)::uuid IS NULL OR cm.merchant_id > sqlc.narg(after)::uuid)
        AND cm.created_at < sqlc.arg(message_cutoff)::timestamptz
      ORDER BY 1 LIMIT sqlc.arg(merchant_limit)::int)
    UNION
    (SELECT DISTINCT lr.merchant_id AS mid
       FROM billing.provider_listed_rebills lr
      WHERE (sqlc.narg(after)::uuid IS NULL OR lr.merchant_id > sqlc.narg(after)::uuid)
        AND lr.listed_at < sqlc.arg(listed_rebill_cutoff)::timestamptz
      ORDER BY 1 LIMIT sqlc.arg(merchant_limit)::int)
    UNION
    (SELECT DISTINCT ut.merchant_id AS mid
       FROM billing.usage_totals ut
      WHERE (sqlc.narg(after)::uuid IS NULL OR ut.merchant_id > sqlc.narg(after)::uuid)
        AND ut.lease_id = '00000000-0000-0000-0000-000000000000'::uuid
        AND ut.period_ends_at < sqlc.arg(usage_totals_cutoff)::timestamptz
      ORDER BY 1 LIMIT sqlc.arg(merchant_limit)::int)
) q
ORDER BY q.mid
LIMIT sqlc.arg(merchant_limit)::int;

-- Row retention. Each statement deletes one bounded batch, oldest first, of
-- one merchant's rows past their period; the cleanup worker loops.

-- Tables that refuse ad-hoc deletes take them only from a transaction that
-- named the table here, and only for rows past the period their trigger
-- declares (billing.guard_retention_delete). Their sweeps count the period on
-- the database clock, the one the trigger reads.
-- name: DeclareRetentionSweep :exec
SELECT set_config('openrails.retention_table', sqlc.arg(table_name)::text, true);

-- name: DeleteSubscriptionTransitionsPastRetention :execrows
DELETE FROM billing.subscription_status_transitions
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND id IN (
    SELECT st.id FROM billing.subscription_status_transitions st
    WHERE st.merchant_id = sqlc.arg(merchant_id)::uuid
      AND st.occurred_at < now() - make_interval(days => sqlc.arg(retention_days)::int)
    ORDER BY st.occurred_at
    LIMIT sqlc.arg(row_limit)::int
);

-- A finding still being seen is kept however long ago it was resolved: an
-- ignored drift that persists must not come back as new.
-- name: DeleteReleasedParksBefore :execrows
DELETE FROM billing.parked_provider_subscriptions
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND id IN (
    SELECT pk.id FROM billing.parked_provider_subscriptions pk
    WHERE pk.merchant_id = sqlc.arg(merchant_id)::uuid
      AND pk.released_at < sqlc.arg(cutoff)::timestamptz
    ORDER BY pk.released_at
    LIMIT sqlc.arg(row_limit)::int
);

-- name: DeleteResolvedFindingsBefore :execrows
DELETE FROM billing.reconciliation_findings
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND id IN (
    SELECT rf.id FROM billing.reconciliation_findings rf
    WHERE rf.merchant_id = sqlc.arg(merchant_id)::uuid
      AND rf.resolved_at IS NOT NULL
      AND GREATEST(rf.resolved_at, rf.last_seen_at) < sqlc.arg(cutoff)::timestamptz
    ORDER BY GREATEST(rf.resolved_at, rf.last_seen_at)
    LIMIT sqlc.arg(row_limit)::int
);

-- An attempt that expired without reaching a provider: no payment,
-- subscription or provider transaction, and no provider intent, Solana Pay
-- reference or receipt names it. Its checkout session, if one still exists,
-- goes with it. A soft-deleted attempt belongs to a destructive run that can
-- still be undone, and is left to it.
-- name: DeleteAbandonedInvoiceAttemptsBefore :execrows
DELETE FROM billing.invoice_attempts
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND deleted_at IS NULL
  AND id IN (
    SELECT ca.id FROM billing.invoice_attempts ca
    WHERE ca.merchant_id = sqlc.arg(merchant_id)::uuid
      AND ca.status = 'expired' AND ca.deleted_at IS NULL
      AND ca.payment_id IS NULL AND ca.subscription_id IS NULL AND ca.transaction_id IS NULL
      AND ca.expires_at < sqlc.arg(cutoff)::timestamptz
      AND NOT EXISTS (SELECT 1 FROM billing.solana_pay_references r WHERE r.merchant_id = ca.merchant_id AND r.invoice_attempt_id = ca.id)
      AND NOT EXISTS (SELECT 1 FROM billing.solana_pay_receipts rc WHERE rc.merchant_id = ca.merchant_id AND rc.invoice_attempt_id = ca.id)
      AND NOT EXISTS (SELECT 1 FROM billing.provider_intents pi WHERE pi.merchant_id = ca.merchant_id AND pi.payload ? 'order_attempt_id' AND pi.payload->>'order_attempt_id' = ca.id::text)
    ORDER BY ca.expires_at
    LIMIT sqlc.arg(row_limit)::int
);

-- Finished intents that only carried an instruction to a provider. An intent
-- that moved or refused money, enrolled a membership or erased a card is the
-- record of that: it is not one of these types and is never deleted here. An
-- operation log entry of a deleted intent keeps its row and its operation id.
-- name: DeleteFinishedOutboxIntentsBefore :execrows
DELETE FROM billing.provider_intents
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND id IN (
    SELECT pi.id FROM billing.provider_intents pi
    WHERE pi.merchant_id = sqlc.arg(merchant_id)::uuid
      AND pi.status IN ('succeeded', 'failed_terminal', 'superseded', 'expired')
      AND pi.destructive_run_id IS NULL
      AND pi.intent_type IN ('nmi_delete_subscription', 'stripe_cancel_subscription', 'stripe_resume_subscription', 'ccbill_cancel_subscription', 'nmi_payment_method_update', 'nmi_payment_source_update', 'nmi_card_vault', 'network_token', 'stripe_archive_price', 'stripe_archive_product', 'solana_sunset_plan', 'account_updater')
      AND pi.updated_at < sqlc.arg(cutoff)::timestamptz
    ORDER BY pi.updated_at
    LIMIT sqlc.arg(row_limit)::int
);

-- name: DeleteOperationLogBefore :execrows
DELETE FROM billing.operation_log
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND id IN (
    SELECT ol.id FROM billing.operation_log ol
    WHERE ol.merchant_id = sqlc.arg(merchant_id)::uuid
      AND ol.occurred_at < sqlc.arg(cutoff)::timestamptz
    ORDER BY ol.occurred_at
    LIMIT sqlc.arg(row_limit)::int
);



-- name: DeleteSubscriptionBillingChangesPastRetention :execrows
DELETE FROM billing.subscription_billing_changes
WHERE merchant_id = sqlc.arg(merchant_id)::uuid
  AND id IN (
    SELECT st.id FROM billing.subscription_billing_changes st
    WHERE st.merchant_id = sqlc.arg(merchant_id)::uuid
      AND st.occurred_at < now() - make_interval(days => sqlc.arg(retention_days)::int)
    ORDER BY st.occurred_at
    LIMIT sqlc.arg(row_limit)::int
);

-- The host outbox is a dedupe journal with no delivery state: every event
-- goes at one cutoff.
-- name: DeleteHostOutboxBefore :execrows
DELETE FROM billing.host_outbox
WHERE ctid IN (
    SELECT ho.ctid FROM billing.host_outbox ho
    WHERE ho.merchant_id = sqlc.arg(merchant_id)::uuid
      AND ho.recorded_at < sqlc.arg(cutoff)::timestamptz
    ORDER BY ho.recorded_at
    LIMIT sqlc.arg(row_limit)::int
);

-- A closed limit window past its retention: one that reached its capacity
-- after reached_cutoff, any other after cutoff; a row another writer holds
-- waits for a later pass.
-- name: DeleteLimitWindowsBefore :execrows
DELETE FROM billing.limit_windows w
WHERE (w.merchant_id, w.customer_id, w.limit_key, w.window_starts_at) IN (
    SELECT e.merchant_id, e.customer_id, e.limit_key, e.window_starts_at
    FROM billing.limit_windows e
    WHERE e.merchant_id = sqlc.arg(merchant_id)::uuid AND e.window_ends_at < sqlc.arg(cutoff)::timestamptz
      AND (e.reached_at IS NULL OR e.window_ends_at < sqlc.arg(reached_cutoff)::timestamptz)
    ORDER BY e.window_ends_at
    LIMIT sqlc.arg(row_limit)::int
    FOR UPDATE SKIP LOCKED
);

-- A lease goes with its holds.
-- name: DeleteLeasesBefore :execrows
DELETE FROM billing.leases l
WHERE (l.merchant_id, l.id) IN (
    SELECT e.merchant_id, e.id
    FROM billing.leases e
    WHERE e.merchant_id = sqlc.arg(merchant_id)::uuid AND e.last_sync_at < sqlc.arg(cutoff)::timestamptz
    ORDER BY e.last_sync_at
    LIMIT sqlc.arg(row_limit)::int
    FOR UPDATE SKIP LOCKED
);

-- A period's running totals once its late usage window closed: its posted
-- rows, unless a row of it is still unposted (its posting waits).
-- name: DeleteClosedUsageTotals :execrows
DELETE FROM billing.usage_totals t
WHERE (t.merchant_id, t.customer_id, t.currency, t.period_starts_at, t.meter_key, t.cell, t.print, t.lapse_at, t.lease_id) IN (
    SELECT e.merchant_id, e.customer_id, e.currency, e.period_starts_at, e.meter_key, e.cell, e.print, e.lapse_at, e.lease_id
    FROM billing.usage_totals e
    WHERE e.merchant_id = sqlc.arg(merchant_id)::uuid AND e.lease_id = '00000000-0000-0000-0000-000000000000'::uuid
      AND e.period_ends_at < sqlc.arg(cutoff)::timestamptz
      AND NOT EXISTS (SELECT 1 FROM billing.usage_totals u
          WHERE u.merchant_id = e.merchant_id AND u.customer_id = e.customer_id AND u.currency = e.currency
            AND u.period_starts_at = e.period_starts_at AND u.lease_id <> '00000000-0000-0000-0000-000000000000'::uuid)
    ORDER BY e.period_ends_at
    LIMIT sqlc.arg(row_limit)::int
    FOR UPDATE SKIP LOCKED
);

-- name: DeleteLimitAdjustmentsBefore :execrows
DELETE FROM billing.limit_adjustments a
WHERE (a.merchant_id, a.customer_id, a.source_id) IN (
    SELECT e.merchant_id, e.customer_id, e.source_id
    FROM billing.limit_adjustments e
    WHERE e.merchant_id = sqlc.arg(merchant_id)::uuid AND e.created_at < sqlc.arg(cutoff)::timestamptz
    ORDER BY e.created_at
    LIMIT sqlc.arg(row_limit)::int
    FOR UPDATE SKIP LOCKED
);

-- name: DeleteCustomerMessagesBefore :execrows
DELETE FROM billing.customer_messages m
WHERE (m.merchant_id, m.id) IN (
    SELECT e.merchant_id, e.id
    FROM billing.customer_messages e
    WHERE e.merchant_id = sqlc.arg(merchant_id)::uuid AND e.created_at < sqlc.arg(cutoff)::timestamptz
    ORDER BY e.created_at
    LIMIT sqlc.arg(row_limit)::int
    FOR UPDATE SKIP LOCKED
);

-- name: DeleteWebhookHealthDaysBefore :execrows
DELETE FROM billing.webhook_health_daily
WHERE ctid IN (
    SELECT e.ctid FROM billing.webhook_health_daily e
    WHERE e.merchant_id = sqlc.arg(merchant_id)::uuid AND e.day_at < sqlc.arg(cutoff)::timestamptz
    ORDER BY e.day_at
    LIMIT sqlc.arg(row_limit)::int
);

-- name: DeleteProviderListedRebillsBefore :execrows
DELETE FROM billing.provider_listed_rebills
WHERE ctid IN (
    SELECT e.ctid FROM billing.provider_listed_rebills e
    WHERE e.merchant_id = sqlc.arg(merchant_id)::uuid AND e.listed_at < sqlc.arg(cutoff)::timestamptz
    ORDER BY e.listed_at
    LIMIT sqlc.arg(row_limit)::int
);

-- The lease of a periodic kind no fleet has scheduled for the period: a kind
-- since removed. A lease that expired is free either way, so deleting one
-- never races a claim.
-- name: DeleteOrphanPeriodicLeases :execrows
DELETE FROM billing.periodic_leases
WHERE kind IN (
    SELECT l.kind FROM billing.periodic_leases l
    WHERE l.expires_at < now() - make_interval(days => sqlc.arg(retention_days)::int)
    ORDER BY l.kind
    LIMIT sqlc.arg(row_limit)::int
);
