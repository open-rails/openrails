-- Known-through per PSP: what OpenRails has read of each PSP, so a PSP it
-- cannot read is reported stale and nothing infers from its silence; and why
-- a subscription is unverified: awaiting its provider (our record is stale)
-- or unconfirmed (its provider was read and could not tell).

CREATE TABLE billing.psp_freshness (
    merchant_id uuid NOT NULL,
    psp_id uuid NOT NULL,
    read_at timestamptz,
    failing_since timestamptz,
    failed_at timestamptz,
    failure text,
    imported_through timestamptz,
    stale_since timestamptz,
    notified_at timestamptz,
    caught_up_at timestamptz,
    catch_up jsonb,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (merchant_id, psp_id),
    CONSTRAINT psp_freshness_psp_id_fkey FOREIGN KEY (merchant_id, psp_id) REFERENCES billing.psps (merchant_id, id) ON DELETE RESTRICT,
    CONSTRAINT psp_freshness_failure_check CHECK ((failing_since IS NULL) = (failure IS NULL) AND (failed_at IS NULL) = (failure IS NULL)
        AND (failing_since IS NULL OR failed_at >= failing_since)
        AND (failure IS NULL OR failure IN ('not_configured', 'credentials_refused', 'unreachable'))),
    CONSTRAINT psp_freshness_notified_check CHECK (notified_at IS NULL OR stale_since IS NOT NULL),
    CONSTRAINT psp_freshness_catch_up_check CHECK ((caught_up_at IS NULL) = (catch_up IS NULL)
        AND (catch_up IS NULL OR jsonb_typeof(catch_up) = 'object'))
);
COMMENT ON TABLE billing.psp_freshness IS 'What OpenRails has read of each PSP, the evidence its known-through is derived from: the last provider read that succeeded, the failure of the reads since, the latest import''s evidence instant, and the stale episode (stale_since) during which nothing infers from the PSP''s silence. A PSP with no row has never been read, imported or found unreadable.';
COMMENT ON COLUMN billing.psp_freshness.read_at IS 'The last provider read of the PSP that succeeded.';
COMMENT ON COLUMN billing.psp_freshness.failing_since IS 'The first failed read since read_at; NULL while reads succeed.';
COMMENT ON COLUMN billing.psp_freshness.failed_at IS 'The last failed read; a stale PSP is tried again an hour after it.';
COMMENT ON COLUMN billing.psp_freshness.failure IS 'Why its reads fail: not_configured (no credentials to read with), credentials_refused (the provider refused them) or unreachable (the provider did not answer).';
COMMENT ON COLUMN billing.psp_freshness.imported_through IS 'The evidence instant (as_of) of the latest declared book naming the PSP.';
COMMENT ON COLUMN billing.psp_freshness.stale_since IS 'When the PSP''s record stopped being complete through now; NULL while it is. The overdue detector and the passes that infer from absence skip its subscriptions until a catch-up read clears it.';
COMMENT ON COLUMN billing.psp_freshness.notified_at IS 'When the stale episode''s psp.stale event and alert were queued, once it reached warning or critical; its end then queues psp.recovered.';
COMMENT ON COLUMN billing.psp_freshness.catch_up IS 'What the last catch-up read found in the gap: from, through, and the renewals paid, failed, canceled and still unconfirmed.';

CREATE INDEX psp_freshness_stale_idx ON billing.psp_freshness (merchant_id, psp_id)
    WHERE stale_since IS NOT NULL;

-- Why a subscription is unverified. It enters awaiting its provider; a read
-- of the provider that cannot tell makes it unconfirmed.
ALTER TABLE billing.subscriptions ADD COLUMN unverified_reason text;
UPDATE billing.subscriptions s
   SET unverified_reason = CASE WHEN EXISTS (SELECT 1 FROM billing.subscription_verifications v
        WHERE v.merchant_id = s.merchant_id AND v.subscription_id = s.id AND v.last_read_at IS NOT NULL AND v.last_error IS NULL)
        THEN 'unconfirmed' ELSE 'awaiting_provider' END
 WHERE s.status = 'unverified';
ALTER TABLE billing.subscriptions ADD CONSTRAINT subscriptions_unverified_reason_check
    CHECK ((status = 'unverified') = (unverified_reason IS NOT NULL) AND (unverified_reason IS NULL OR unverified_reason IN ('awaiting_provider', 'unconfirmed')));
COMMENT ON COLUMN billing.subscriptions.unverified_reason IS 'Why an unverified subscription is: awaiting_provider (no read of its provider since it became unverified: our record is stale) or unconfirmed (its provider was read and could not tell). NULL unless unverified.';

CREATE FUNCTION billing.subscriptions_unverified_reason() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.status <> 'unverified' THEN
        NEW.unverified_reason := NULL;
    ELSIF TG_OP = 'INSERT' OR OLD.status <> 'unverified' THEN
        NEW.unverified_reason := COALESCE(NEW.unverified_reason, 'awaiting_provider');
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER subscriptions_unverified_reason BEFORE INSERT OR UPDATE OF status, unverified_reason ON billing.subscriptions
    FOR EACH ROW EXECUTE FUNCTION billing.subscriptions_unverified_reason();

-- Awaiting its provider is our staleness, not a risk to the merchant's MRR.
CREATE OR REPLACE FUNCTION billing.subscription_risk(s billing.subscriptions, p_level bigint, p_prev text[]) RETURNS text[]
LANGUAGE sql IMMUTABLE SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
SELECT CASE
    WHEN p_level = 0 OR s.deleted_at IS NOT NULL THEN '{}'::text[]
    WHEN s.status = 'canceled' THEN p_prev
    ELSE array_remove(ARRAY[
        CASE WHEN s.status = 'past_due' THEN 'past_due' END,
        CASE WHEN s.status = 'awaiting_method' THEN 'needs_payment_method' END,
        CASE WHEN s.status = 'unverified' AND s.unverified_reason = 'unconfirmed' THEN 'unverified' END]::text[], NULL)
END
$$;
COMMENT ON FUNCTION billing.subscription_risk(s billing.subscriptions, p_level bigint, p_prev text[]) IS 'The reasons a subscription adding MRR is at risk, from its status: past_due, needs_payment_method (awaiting_method; a provider''s past due is past_due) and unverified when a read left it unconfirmed (awaiting its provider is not at risk). Canceled at its period end, it keeps p_prev.';

-- A read that makes an unverified subscription unconfirmed changes its risk.
CREATE FUNCTION billing.subscriptions_track_mrr_risk() RETURNS trigger
LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
BEGIN
    IF NEW.price_id IS NULL OR billing.billing_restore_active(NEW.merchant_id) THEN
        RETURN NULL;
    END IF;
    PERFORM billing.refresh_subscription_mrr(NEW, NEW.updated_at, 'renewed');
    RETURN NULL;
END;
$$;
CREATE TRIGGER subscriptions_track_mrr_risk AFTER UPDATE OF unverified_reason ON billing.subscriptions
    FOR EACH ROW WHEN (OLD.status = NEW.status AND OLD.unverified_reason IS DISTINCT FROM NEW.unverified_reason)
    EXECUTE FUNCTION billing.subscriptions_track_mrr_risk();

-- The cases only our stale record opened are no risk the merchant ran: they
-- close superseded, which no recovery rate counts, and leave no reason behind.
WITH closed AS (
    UPDATE billing.mrr_risk_cases c SET closed_at = greatest(now(), c.opened_at), outcome = 'superseded', mrr_at_exit = sm.recorded_mrr
      FROM billing.subscription_mrr sm
      JOIN billing.subscriptions s ON s.merchant_id = sm.merchant_id AND s.id = sm.subscription_id
     WHERE sm.merchant_id = c.merchant_id AND sm.subscription_id = c.subscription_id AND c.closed_at IS NULL
       AND s.unverified_reason = 'awaiting_provider' AND sm.recorded_risk <@ ARRAY['unverified']::text[]
    RETURNING c.merchant_id, c.id, c.closed_at, c.mrr_at_exit
)
UPDATE billing.mrr_risk_episodes e SET left_at = closed.closed_at, mrr_at_exit = closed.mrr_at_exit
  FROM closed
 WHERE e.merchant_id = closed.merchant_id AND e.case_id = closed.id AND e.left_at IS NULL;
UPDATE billing.subscription_mrr sm
   SET risk = array_remove(sm.risk, 'unverified'), recorded_risk = array_remove(sm.recorded_risk, 'unverified')
  FROM billing.subscriptions s
 WHERE s.merchant_id = sm.merchant_id AND s.id = sm.subscription_id AND s.unverified_reason = 'awaiting_provider'
   AND (sm.risk @> ARRAY['unverified']::text[] OR sm.recorded_risk @> ARRAY['unverified']::text[]);
COMMENT ON COLUMN billing.subscription_mrr.risk IS 'Why the subscription''s MRR is at risk now from its status: past_due, needs_payment_method and unverified (unconfirmed: awaiting its provider is none), in that order; empty when it is not at risk or adds no MRR. Canceled at its period end, it keeps them until its MRR ends. card_expiring and delinquent are read from state (billing.mrr_risk_now).';

-- A PSP's stale episode and its end are host events.
ALTER TABLE billing.host_outbox DROP CONSTRAINT host_outbox_currency_check, DROP CONSTRAINT host_outbox_payload_check;
ALTER TABLE billing.host_outbox ADD CONSTRAINT host_outbox_currency_check CHECK (
        (currency IS NULL) = (event_type IN ('product.entitlements_changed', 'customer.network_linked', 'customer.network_unlinked', 'mandate.revoked', 'destructive.hold_opened', 'destructive.hold_cleared', 'psp.stale', 'psp.recovered'))
        AND (currency IS NULL OR currency ~ '^[A-Z0-9]{3,12}$')
    ),
    ADD CONSTRAINT host_outbox_payload_check CHECK (
        (event_type = 'payment.settled' AND subject_type = 'payment' AND payment_id IS NOT NULL AND subject_id = payment_id AND amount IS NOT NULL AND amount > 0 AND data = '{}'::jsonb)
        OR (event_type IN ('delinquency.grace', 'delinquency.entered', 'delinquency.cleared', 'balance.low', 'balance.depleted') AND subject_type = 'customer' AND payment_id IS NULL AND amount IS NULL)
        OR (event_type = 'product.entitlements_changed' AND subject_type = 'product' AND payment_id IS NULL AND amount IS NULL)
        OR (event_type IN ('invoice.completed', 'invoice.requires_action', 'invoice.payment_failed', 'invoice.canceled', 'invoice.expired', 'invoice.voided') AND subject_type = 'invoice' AND payment_id IS NULL AND amount IS NOT NULL AND amount >= 0 AND currency IS NOT NULL)
        OR (event_type IN ('customer.network_linked', 'customer.network_unlinked') AND subject_type = 'customer' AND payment_id IS NULL AND amount IS NULL)
        OR (event_type = 'mandate.revoked' AND subject_type = 'mandate' AND payment_id IS NULL AND amount IS NULL)
        OR (event_type IN ('destructive.hold_opened', 'destructive.hold_cleared') AND subject_type = 'destructive_hold' AND payment_id IS NULL AND amount IS NULL AND currency IS NULL AND jsonb_typeof(data) = 'object')
        OR (event_type IN ('psp.stale', 'psp.recovered') AND subject_type = 'psp' AND payment_id IS NULL AND amount IS NULL AND currency IS NULL AND jsonb_typeof(data) = 'object')
    );

CREATE OR REPLACE FUNCTION billing.event_subject_id(kind text, id uuid) RETURNS text
LANGUAGE sql IMMUTABLE PARALLEL SAFE SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
    SELECT CASE WHEN id IS NULL THEN NULL ELSE
      CASE kind WHEN 'payment' THEN 'pay_' WHEN 'product' THEN 'prod_'
      WHEN 'subscription' THEN 'sub_' WHEN 'invoice' THEN 'in_'
      WHEN 'mandate' THEN 'mdt_' WHEN 'destructive_hold' THEN 'dsh_' WHEN 'psp' THEN 'psp_' ELSE '' END || id::text END
$$;

-- The restore guard names the new table (0015's list and it).
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
                  'bank_accounts',
                  'books_starts',
                  'carts',
                  'catalog_applications',
                  'catalog_field_owners',
                  'catalog_limits',
                  'catalog_meters',
                  'catalog_product_limits',
                  'catalog_rate_cards',
                  'credit_grant_events',
                  'credit_grants',
                  'credit_reversal_shares',
                  'custodians',
                  'customer_delinquency',
                  'customer_invoice_profiles',
                  'customer_limit_overrides',
                  'customer_low_balance_lines',
                  'customer_messages',
                  'customer_paid_totals',
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
                  'invoice_attempts',
                  'invoice_lines',
                  'invoices',
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
                  'mrr_movement_lines',
                  'mrr_movements',
                  'mrr_risk_cases',
                  'mrr_risk_episodes',
                  'nmi_bulk_checkpoints',
                  'notification_events',
                  'operation_log',
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
                  'provider_listed_rebills',
                  'provider_schedules',
                  'psp_customers',
                  'psp_freshness',
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
                  'stripe_payout_reads',
                  'subscription_billing_changes',
                  'subscription_cancellations',
                  'subscription_mrr',
                  'subscription_status_transitions',
                  'subscription_verifications',
                  'subscriptions',
                  'usage_card_prints',
                  'usage_pending',
                  'usage_receipts',
                  'usage_totals',
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
