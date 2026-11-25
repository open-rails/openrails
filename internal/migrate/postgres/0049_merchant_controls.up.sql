-- One row of operator controls per merchant replaces its write posture, its
-- destructive-action policy, the deployment's destructive switch and the
-- boot-time engine admission hold: how much OpenRails writes to the
-- merchant's providers, whether the charges OpenRails initiates for it wait,
-- and whether its destructive convergence runs. No row is writes full,
-- rebills released and destructive actions disarmed.
CREATE TABLE billing.merchant_controls (
    merchant_id uuid NOT NULL REFERENCES billing.merchants (id) ON DELETE RESTRICT,
    writes text NOT NULL CHECK (writes IN ('full', 'limited', 'readonly')),
    rebills_held boolean NOT NULL,
    destructive_armed boolean NOT NULL,
    first_pull_completed_at timestamptz,
    changed_by text CHECK (changed_by <> ''),
    reason text CHECK (reason <> ''),
    changed_at timestamptz,
    PRIMARY KEY (merchant_id),
    CONSTRAINT merchant_controls_change_check CHECK ((changed_by IS NULL) = (reason IS NULL) AND (reason IS NULL) = (changed_at IS NULL))
);
COMMENT ON TABLE billing.merchant_controls IS 'One merchant''s operator controls; no row is writes full, rebills released, destructive actions disarmed. A copy of the billing book writes nothing whatever writes says. Export sets the source''s writes readonly and restore the destination''s; each change is a change_log entry (object_type controls).';
COMMENT ON COLUMN billing.merchant_controls.writes IS 'Provider writes: full, limited (what the system initiates waits) or readonly (nothing reaches a provider).';
COMMENT ON COLUMN billing.merchant_controls.rebills_held IS 'The rebill brake: merchant-initiated charges (renewals and rebills OpenRails charges, dunning retries, scheduled usage collection) wait until it is released; customer-initiated charges never wait.';
COMMENT ON COLUMN billing.merchant_controls.destructive_armed IS 'Destructive convergence (cancellation, revocation, provider deletes, pull overwrites) runs only when armed; arm once the merchant''s first pull has been reviewed.';
COMMENT ON COLUMN billing.merchant_controls.first_pull_completed_at IS 'When a completed pull first surveyed the merchant, so its findings are ready to review; a destructive decision rests on no older evidence. Stamped once, never a control.';
COMMENT ON COLUMN billing.merchant_controls.changed_by IS 'Who made the last change, with its reason and instant; NULL until a control changes.';

-- A merchant keeps its posture's writes, and is armed only where the switch
-- was on, its own stop off and its enforcement armed. The last change is the
-- newer of its posture's and its policy's.
INSERT INTO billing.merchant_controls (merchant_id, writes, rebills_held, destructive_armed, first_pull_completed_at, changed_by, reason, changed_at)
SELECT m.id, COALESCE(p.mode, 'full'), false,
    COALESCE(s.enabled, false) AND COALESCE(d.destructive_actions_enabled, true) AND d.enforce_armed_at IS NOT NULL,
    d.first_pull_completed_at, latest.changed_by, latest.reason, latest.changed_at
FROM billing.merchants m
LEFT JOIN billing.merchant_write_posture p ON p.merchant_id = m.id
LEFT JOIN billing.merchant_destructive_policy d ON d.merchant_id = m.id
LEFT JOIN billing.destructive_action_switch s ON true
LEFT JOIN LATERAL (
    SELECT v.changed_by, COALESCE(v.reason, 'migrated') AS reason, v.changed_at
    FROM (VALUES
        (p.set_by, CASE p.reason WHEN 'exported' THEN 'billing export' WHEN 'restored' THEN 'billing restore' ELSE 'write posture' END, p.set_at),
        (NULLIF(btrim(d.updated_by), ''), NULLIF(btrim(d.reason), ''), d.updated_at)) v (changed_by, reason, changed_at)
    WHERE v.changed_by IS NOT NULL
    ORDER BY v.changed_at DESC
    LIMIT 1) latest ON true
WHERE p.merchant_id IS NOT NULL OR d.merchant_id IS NOT NULL;

DROP TABLE billing.merchant_write_posture;
DROP TABLE billing.merchant_destructive_policy;
DROP TABLE billing.destructive_action_switch;

-- What a dispute response waits for changing wakes the waiting responses.
CREATE TRIGGER merchant_controls_wake_dispute_responses AFTER INSERT OR UPDATE OF writes, destructive_armed ON billing.merchant_controls
    FOR EACH STATEMENT EXECUTE FUNCTION billing.wake_dispute_responses();

-- A change of controls is configuration history.
ALTER TABLE billing.change_log DROP CONSTRAINT change_log_object_type_check;
ALTER TABLE billing.change_log ADD CONSTRAINT change_log_object_type_check CHECK (object_type IN ('product', 'price', 'meter', 'limit', 'merchant', 'messages', 'psp', 'custodian', 'controls'));

-- A restore lands on a merchant never armed nor pulled; it sets the
-- destination's writes readonly itself.
CREATE OR REPLACE FUNCTION billing.guard_billing_restore_receipt() RETURNS trigger
LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $_$
DECLARE item record; occupied boolean;
BEGIN
    IF TG_OP='DELETE' THEN
        IF OLD.kind='billing_restore' THEN
            RAISE EXCEPTION 'billing restore receipts are immutable' USING ERRCODE='23514';
        END IF;
        RETURN OLD;
    END IF;
    IF NEW.kind<>'billing_restore' AND (TG_OP='INSERT' OR OLD.kind<>'billing_restore') THEN RETURN NEW; END IF;
    IF NEW.merchant_id IS DISTINCT FROM billing.current_merchant_id() THEN
        RAISE EXCEPTION 'billing restore merchant mismatch' USING ERRCODE='42501';
    END IF;
    -- The merchant row is the serialization point used by begin_billing_restore
    -- and by FK-backed first writes. No database-owner exemption or GUC-only
    -- permission can create a receipt for an occupied destination.
    PERFORM 1 FROM billing.merchants WHERE id=NEW.merchant_id AND status='active' AND deleted_at IS NULL FOR UPDATE;
    IF NOT FOUND THEN RAISE EXCEPTION 'billing restore merchant missing or inactive' USING ERRCODE='P0002'; END IF;
    IF TG_OP='INSERT' THEN
        IF NEW.status<>'running' OR NEW.finished_at IS NOT NULL OR NEW.summary IS NOT NULL THEN
            RAISE EXCEPTION 'billing restore receipts must begin running and unfinished' USING ERRCODE='23514';
        END IF;
        FOR item IN SELECT c.relname FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
            JOIN pg_attribute a ON a.attrelid=c.oid AND a.attname='merchant_id' AND NOT a.attisdropped
            WHERE n.nspname=TG_TABLE_SCHEMA AND c.relkind IN ('r','p')
              -- Every merchant-scoped table but the directory's own identity rows
              -- (former names, host claims); a test keeps this list complete.
              AND c.relname = ANY(ARRAY[
                  'balance_watches',
                  'books_starts',
                  'carts',
                  'catalog_applications',
                  'catalog_field_owners',
                  'catalog_limits',
                  'catalog_meters',
                  'catalog_product_limits',
                  'catalog_rate_cards',
                  'catalog_restore_receipts',
                  'credit_grant_events',
                  'credit_grants',
                  'credit_reversal_shares',
                  'custodians',
                  'customer_delinquency',
                  'customer_invoice_profiles',
                  'customer_limit_overrides',
                  'customer_low_balance_lines',
                  'customer_messages',
                  'customer_mrr',
                  'customers',
                  'dashboard_configs',
                  'destructive_actions',
                  'destructive_holds',
                  'destructive_run_before_images',
                  'disputes',
                  'eventlog_events',
                  'fraud_reports',
                  'grants',
                  'host_outbox',
                  'idempotency_keys',
                  'lease_in_flight',
                  'leases',
                  'ledger_accounts',
                  'ledger_daily',
                  'ledger_rollup_pending',
                  'ledger_transfers',
                  'ledger_type_totals',
                  'ledger_units',
                  'limit_adjustments',
                  'limit_windows',
                  'maintenance_runs',
                  'mandates',
                  'merchant_leases',
                  'money_settings',
                  'mrr_cohort_flows',
                  'mrr_customer_flows',
                  'mrr_customer_totals',
                  'mrr_flows',
                  'mrr_movement_lines',
                  'mrr_movements',
                  'mrr_product_flows',
                  'mrr_product_totals',
                  'mrr_risk_cases',
                  'mrr_risk_due',
                  'mrr_risk_episodes',
                  'mrr_risk_flows',
                  'mrr_risk_totals',
                  'mrr_scheduled',
                  'mrr_streak_flows',
                  'mrr_totals',
                  'nmi_bulk_checkpoints',
                  'nmi_history_months',
                  'nmi_history_reads',
                  'notification_events',
                  'operation_log',
                  'order_attempts',
                  'order_lines',
                  'orders',
                  'owed_items',
                  'ownership_claims',
                  'payment_attempts',
                  'payment_method_versions',
                  'payment_methods',
                  'payments',
                  'price_keys',
                  'price_migrations',
                  'price_psp_bindings',
                  'prices',
                  'product_access',
                  'product_entitlements',
                  'products',
                  'provider_bills',
                  'provider_charge_attributions',
                  'provider_cycle_receipts',
                  'provider_intents',
                  'provider_schedules',
                  'psp_customers',
                  'psp_refresh_watermarks',
                  'psps',
                  'rebill_cycles',
                  'reconciliation_findings',
                  'reconciliation_state',
                  'revenue_schedules',
                  'running_charges',
                  'scheduled_changes',
                  'solana_pay_receipts',
                  'solana_pay_references',
                  'solana_subscriptions',
                  'stored_card_checks',
                  'stripe_fee_reads',
                  'subscription_billing_changes',
                  'subscription_cancellations',
                  'subscription_mrr',
                  'subscription_status_transitions',
                  'subscription_verifications',
                  'subscriptions',
                  'usage_card_prints',
                  'usage_postings',
                  'usage_unposted',
                  'webhook_events',
                  'webhook_health',
                  'webhook_health_daily']::text[])
        LOOP
            EXECUTE format('SELECT EXISTS (SELECT 1 FROM %I.%I WHERE merchant_id=$1)',TG_TABLE_SCHEMA,item.relname)
                INTO occupied USING NEW.merchant_id;
            IF occupied THEN RAISE EXCEPTION 'billing restore destination is not empty: %',item.relname USING ERRCODE='55000'; END IF;
        END LOOP;
        IF EXISTS (SELECT 1 FROM billing.merchant_controls c WHERE c.merchant_id=NEW.merchant_id
            AND (c.destructive_armed OR c.first_pull_completed_at IS NOT NULL)) THEN
            RAISE EXCEPTION 'billing restore destination is not empty: merchant_controls' USING ERRCODE='55000';
        END IF;
    ELSE
        IF OLD.kind<>'billing_restore' OR NEW.kind<>'billing_restore'
           OR OLD.status<>'running' OR NEW.status<>'completed'
           OR NEW.finished_at IS NULL
           OR (to_jsonb(NEW)-ARRAY['status','finished_at','summary','run_class']) IS DISTINCT FROM (to_jsonb(OLD)-ARRAY['status','finished_at','summary','run_class'])
           OR NOT EXISTS (SELECT 1 FROM billing.maintenance_runs r WHERE r.id=OLD.id AND r.merchant_id=OLD.merchant_id
               AND r.xmin=pg_current_xact_id_if_assigned()::xid)
           OR OLD.id::text IS DISTINCT FROM current_setting('openrails.billing_restore_id',true)
           OR NEW.summary->>'digest' IS NULL OR NEW.summary->>'digest' !~ '^[0-9a-f]{64}$'
           OR jsonb_typeof(NEW.summary->'rows') IS DISTINCT FROM 'number'
           OR (NEW.summary->>'rows')::numeric < 0
           OR (NEW.summary->>'rows')::numeric <> trunc((NEW.summary->>'rows')::numeric) THEN
            RAISE EXCEPTION 'invalid billing restore receipts transition' USING ERRCODE='23514';
        END IF;
    END IF;
    RETURN NEW;
END;
$_$;
