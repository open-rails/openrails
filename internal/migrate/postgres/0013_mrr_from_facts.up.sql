-- MRR is read from its facts: movements, their lines, and the at-risk cases
-- and episodes. The rollups, the scheduled table, the customers' totals and
-- the risk queue go. A cancellation at period end is a movement dated at its
-- end, written when it is decided; nothing runs at the end. card_expiring and
-- delinquent are read from state, for now only.

-- What kept the rollups, the schedule and the risk queue.
DROP TRIGGER subscription_mrr_settle ON billing.subscription_mrr;
DROP TRIGGER scheduled_changes_track_mrr ON billing.scheduled_changes;
DROP FUNCTION billing.scheduled_changes_track_mrr();
DROP TRIGGER payment_methods_track_mrr_risk ON billing.payment_methods;
DROP FUNCTION billing.payment_methods_track_mrr_risk();
DROP TRIGGER money_settings_track_mrr_risk ON billing.money_settings;
DROP FUNCTION billing.money_settings_track_mrr_risk();
DROP TRIGGER customer_delinquency_track_mrr_risk ON billing.customer_delinquency;
DROP FUNCTION billing.customer_delinquency_track_mrr_risk();
DROP TRIGGER payment_attempts_track_mrr_risk ON billing.payment_attempts;
DROP FUNCTION billing.payment_attempts_track_mrr_risk();
DROP FUNCTION billing.queue_mrr_risk(uuid, uuid, timestamptz);
DROP FUNCTION billing.effect_due_mrr(uuid, uuid, timestamptz);
DROP FUNCTION billing.effect_due_mrr_risk(uuid, integer, uuid);
DROP TRIGGER immutable_mrr_movements ON billing.mrr_movements;
DROP TRIGGER immutable_mrr_movement_lines ON billing.mrr_movement_lines;

-- A restore no longer proves the rollups.
CREATE OR REPLACE FUNCTION billing.require_finished_billing_restore() RETURNS trigger
LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
BEGIN
    IF NEW.kind='billing_restore' THEN
        IF NOT EXISTS (SELECT 1 FROM billing.maintenance_runs WHERE id=NEW.id AND merchant_id=NEW.merchant_id AND status='completed') THEN
            RAISE EXCEPTION 'unfinished billing restore receipts cannot commit' USING ERRCODE='23514';
        END IF;
        PERFORM billing.check_billing_restore_ledger(NEW.merchant_id);
        PERFORM billing.check_billing_restore_credit_grants(NEW.merchant_id);
    END IF;
    RETURN NULL;
END;
$$;
DROP FUNCTION billing.check_billing_restore_mrr(uuid);

-- The restore guard no longer names the tables that go.
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

-- Movements gain the scheduled churn and the customer-level logo change;
-- lines carry their movement's instant and currency, or stand alone.
ALTER TABLE billing.mrr_movements ADD COLUMN scheduled boolean, ADD COLUMN customer_logo_delta integer;
ALTER TABLE billing.mrr_movement_lines ADD COLUMN occurred_at timestamptz, ADD COLUMN currency text,
    ALTER COLUMN movement_id DROP NOT NULL;
ALTER TABLE billing.subscription_mrr ADD COLUMN recorded_ends_at timestamptz;

UPDATE billing.mrr_movements SET scheduled = false;
UPDATE billing.mrr_movement_lines l SET occurred_at = v.occurred_at, currency = v.currency
  FROM billing.mrr_movements v
 WHERE v.merchant_id = l.merchant_id AND v.id = l.movement_id;

-- Each churn still scheduled becomes a movement dated at its end, classified
-- along the customer's timeline from its MRR now; one due and not yet taken
-- is dated at its end too.
WITH e AS (
    SELECT ms.merchant_id, ms.customer_id, ms.currency, ms.effective_at, sum(ms.mrr_delta)::bigint AS delta,
           (array_agg(ms.cause ORDER BY ms.mrr_delta, ms.subscription_id))[1] AS cause
      FROM billing.mrr_scheduled ms
     WHERE ms.status = 'scheduled' AND ms.kind = 'churn'
     GROUP BY ms.merchant_id, ms.customer_id, ms.currency, ms.effective_at
), r AS (
    SELECT e.*, cm.first_active_at,
           cm.mrr + sum(e.delta) OVER (PARTITION BY e.merchant_id, e.customer_id, e.currency ORDER BY e.effective_at) AS after
      FROM e
      JOIN billing.customer_mrr cm ON cm.merchant_id = e.merchant_id AND cm.customer_id = e.customer_id AND cm.currency = e.currency
)
INSERT INTO billing.mrr_movements (merchant_id, customer_id, currency, kind, delta, mrr_before, mrr_after, logo_delta,
    cohort_at, cause, voluntary, occurred_at, scheduled)
SELECT r.merchant_id, r.customer_id, r.currency, CASE WHEN r.after = 0 THEN 'churn' ELSE 'contraction' END, r.delta, r.after - r.delta, r.after,
       -(r.after = 0)::integer, r.first_active_at, r.cause, CASE WHEN r.after = 0 THEN r.cause IN ('canceled', 'merchant_canceled') END,
       r.effective_at, true
  FROM r;
INSERT INTO billing.mrr_movement_lines (merchant_id, movement_id, subscription_id, product_id, price_id, delta, mrr_before, mrr_after,
    subscriber_delta, cause, occurred_at, currency)
SELECT ms.merchant_id, v.id, ms.subscription_id, sm.product_id, sm.price_id, ms.mrr_delta, -ms.mrr_delta, 0, -1, ms.cause, ms.effective_at, ms.currency
  FROM billing.mrr_scheduled ms
  JOIN billing.subscription_mrr sm ON sm.merchant_id = ms.merchant_id AND sm.subscription_id = ms.subscription_id
  JOIN billing.mrr_movements v ON v.merchant_id = ms.merchant_id AND v.customer_id = ms.customer_id AND v.currency = ms.currency
   AND v.occurred_at = ms.effective_at AND v.scheduled
 WHERE ms.status = 'scheduled' AND ms.kind = 'churn';
UPDATE billing.subscription_mrr SET recorded_ends_at = ends_at WHERE ends_at IS NOT NULL;

-- A customer gaining MRR in a first currency, or losing it in its last.
UPDATE billing.mrr_movements v SET customer_logo_delta = x.d
  FROM (SELECT y.merchant_id, y.id, (y.n > 0)::integer - (y.n - y.logo_delta > 0)::integer AS d
          FROM (SELECT merchant_id, id, logo_delta,
                       sum(logo_delta) OVER (PARTITION BY merchant_id, customer_id ORDER BY occurred_at, id) AS n
                  FROM billing.mrr_movements) y) x
 WHERE v.merchant_id = x.merchant_id AND v.id = x.id;

ALTER TABLE billing.mrr_movements
    ALTER COLUMN scheduled SET NOT NULL,
    ALTER COLUMN customer_logo_delta SET NOT NULL,
    ADD CONSTRAINT mrr_movements_customer_logo_delta_check CHECK (abs(customer_logo_delta) <= 1),
    ADD CONSTRAINT mrr_movements_scheduled_check CHECK (NOT scheduled OR kind IN ('churn', 'contraction'));
ALTER TABLE billing.mrr_movement_lines
    ALTER COLUMN occurred_at SET NOT NULL,
    ALTER COLUMN currency SET NOT NULL,
    ADD CONSTRAINT mrr_movement_lines_currency_check CHECK (currency ~ '^[A-Z0-9]{3,12}$');

-- Each case's at-risk MRR whatever its reasons: an episode of its own.
ALTER TABLE billing.mrr_risk_episodes DROP CONSTRAINT mrr_risk_episodes_reason_check,
    ADD CONSTRAINT mrr_risk_episodes_reason_check CHECK (reason IN ('past_due', 'needs_payment_method', 'unverified', 'card_expiring', 'delinquent', 'any'));
INSERT INTO billing.mrr_risk_episodes (merchant_id, case_id, subscription_id, currency, reason, entered_at, mrr_at_entry, left_at, mrr_at_exit)
SELECT c.merchant_id, c.id, c.subscription_id, c.currency, 'any', c.opened_at, c.mrr_at_entry, c.closed_at, c.mrr_at_exit
  FROM billing.mrr_risk_cases c;

-- card_expiring and delinquent keep their history to now and no more: their
-- open episodes end here, and a case they alone kept open is superseded,
-- which no recovery rate counts.
UPDATE billing.mrr_risk_episodes e SET left_at = greatest(now(), e.entered_at), mrr_at_exit = sm.recorded_mrr
  FROM billing.subscription_mrr sm
 WHERE sm.merchant_id = e.merchant_id AND sm.subscription_id = e.subscription_id
   AND e.left_at IS NULL AND e.reason IN ('card_expiring', 'delinquent');
WITH closed AS (
    UPDATE billing.mrr_risk_cases c SET closed_at = greatest(now(), c.opened_at), outcome = 'superseded', mrr_at_exit = sm.recorded_mrr
      FROM billing.subscription_mrr sm
     WHERE sm.merchant_id = c.merchant_id AND sm.subscription_id = c.subscription_id AND c.closed_at IS NULL
       AND NOT sm.recorded_risk && ARRAY['past_due', 'needs_payment_method', 'unverified']::text[]
    RETURNING c.merchant_id, c.id, c.closed_at, c.mrr_at_exit
)
UPDATE billing.mrr_risk_episodes e SET left_at = closed.closed_at, mrr_at_exit = closed.mrr_at_exit
  FROM closed
 WHERE e.merchant_id = closed.merchant_id AND e.case_id = closed.id AND e.left_at IS NULL;
-- A case open on a cancellation at period end closes churned at the end, as
-- settlements now write it at the cancel.
WITH ending AS (
    UPDATE billing.mrr_risk_cases c SET closed_at = greatest(sm.ends_at, c.opened_at), outcome = 'churned', mrr_at_exit = sm.mrr,
           voluntary = COALESCE(s.cancel_type IN ('user', 'merchant'), false)
      FROM billing.subscription_mrr sm
      JOIN billing.subscriptions s ON s.merchant_id = sm.merchant_id AND s.id = sm.subscription_id
     WHERE sm.merchant_id = c.merchant_id AND sm.subscription_id = c.subscription_id AND c.closed_at IS NULL
       AND sm.ends_at IS NOT NULL AND sm.mrr > 0
    RETURNING c.merchant_id, c.id, c.closed_at, c.mrr_at_exit
)
UPDATE billing.mrr_risk_episodes e SET left_at = ending.closed_at, mrr_at_exit = ending.mrr_at_exit
  FROM ending
 WHERE e.merchant_id = ending.merchant_id AND e.case_id = ending.id AND e.left_at IS NULL;

-- subscription_mrr: the status reasons alone, its end recorded, nothing due.
DROP INDEX billing.subscription_mrr_next_change_at_idx;
DROP INDEX billing.subscription_mrr_dirty_idx;
DROP INDEX billing.subscription_mrr_risk_idx;
ALTER TABLE billing.subscription_mrr
    DROP COLUMN next_change_at,
    DROP COLUMN dirty,
    DROP CONSTRAINT subscription_mrr_risk_check,
    DROP CONSTRAINT subscription_mrr_recorded_risk_check;
UPDATE billing.subscription_mrr
   SET risk = ARRAY(SELECT r FROM unnest(risk) WITH ORDINALITY AS x(r, i) WHERE r IN ('past_due', 'needs_payment_method', 'unverified') ORDER BY i),
       recorded_risk = ARRAY(SELECT r FROM unnest(recorded_risk) WITH ORDINALITY AS x(r, i) WHERE r IN ('past_due', 'needs_payment_method', 'unverified') ORDER BY i)
 WHERE risk && ARRAY['card_expiring', 'delinquent']::text[] OR recorded_risk && ARRAY['card_expiring', 'delinquent']::text[];
ALTER TABLE billing.subscription_mrr
    ADD COLUMN dirty boolean NOT NULL GENERATED ALWAYS AS (mrr <> recorded_mrr OR currency <> recorded_currency
        OR product_id <> recorded_product_id OR price_id <> recorded_price_id OR ends_at IS DISTINCT FROM recorded_ends_at
        OR risk <> recorded_risk) STORED,
    ADD CONSTRAINT subscription_mrr_risk_check CHECK (risk <@ ARRAY['past_due', 'needs_payment_method', 'unverified']::text[] AND (risk = '{}' OR mrr > 0)),
    ADD CONSTRAINT subscription_mrr_recorded_risk_check CHECK (recorded_risk <@ ARRAY['past_due', 'needs_payment_method', 'unverified']::text[] AND (recorded_risk = '{}' OR recorded_mrr > 0));
CREATE INDEX subscription_mrr_dirty_idx ON billing.subscription_mrr (merchant_id, customer_id)
    WHERE dirty;
CREATE INDEX subscription_mrr_ends_at_idx ON billing.subscription_mrr (merchant_id, ends_at)
    WHERE ends_at IS NOT NULL;

-- The rollups, the schedule, the customers' totals and the risk queue.
DROP TABLE billing.mrr_flows, billing.mrr_totals, billing.mrr_product_flows, billing.mrr_product_totals, billing.mrr_cohort_flows,
    billing.mrr_streak_flows, billing.mrr_customer_flows, billing.mrr_customer_totals, billing.mrr_risk_flows, billing.mrr_risk_totals,
    billing.mrr_risk_due;
DROP FUNCTION billing.notify_mrr_risk_due();
DROP FUNCTION billing.resolve_mrr_scheduled(billing.mrr_scheduled, text, timestamptz);
DROP FUNCTION billing.schedule_mrr_change(uuid, uuid, timestamptz);
DROP FUNCTION billing.pending_mrr_lines(uuid, uuid, text);
DROP TABLE billing.mrr_scheduled, billing.customer_mrr;
DROP FUNCTION billing.mrr_bucket(timestamptz);

-- Reads by instant: index-only range scans of a merchant's facts.
DROP INDEX billing.mrr_movements_occurred_at_idx;
CREATE INDEX mrr_movements_occurred_at_idx ON billing.mrr_movements (merchant_id, occurred_at)
    INCLUDE (currency, kind, delta, logo_delta, customer_logo_delta, customer_id, voluntary, cause, scheduled, id);
CREATE INDEX mrr_movements_cohort_at_idx ON billing.mrr_movements (merchant_id, cohort_at)
    INCLUDE (currency, occurred_at, delta, logo_delta, customer_id);
CREATE INDEX mrr_movement_lines_occurred_at_idx ON billing.mrr_movement_lines (merchant_id, occurred_at)
    INCLUDE (currency, product_id, delta, subscriber_delta);
DROP INDEX billing.mrr_risk_cases_closed_at_idx;
CREATE INDEX mrr_risk_cases_closed_at_idx ON billing.mrr_risk_cases (merchant_id, closed_at)
    INCLUDE (currency, entry_reason, outcome, mrr_at_exit) WHERE closed_at IS NOT NULL;
CREATE INDEX mrr_risk_episodes_entered_at_idx ON billing.mrr_risk_episodes (merchant_id, entered_at)
    INCLUDE (currency, reason, mrr_at_entry);
CREATE INDEX mrr_risk_episodes_left_at_idx ON billing.mrr_risk_episodes (merchant_id, left_at)
    INCLUDE (currency, reason, mrr_at_entry) WHERE left_at IS NOT NULL;

-- A movement is never rewritten; a scheduled one is deleted when a later
-- settlement writes the customer's ends again, with its lines.
CREATE FUNCTION billing.guard_mrr_movement() RETURNS trigger
LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
BEGIN
    IF TG_OP = 'DELETE' AND OLD.scheduled THEN
        RETURN OLD;
    END IF;
    RAISE EXCEPTION '% is not permitted on immutable billing facts in %', TG_OP, TG_TABLE_NAME USING ERRCODE='23514';
END;
$$;
CREATE FUNCTION billing.guard_mrr_movement_line() RETURNS trigger
LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
BEGIN
    IF TG_OP = 'DELETE' AND EXISTS (SELECT 1 FROM billing.mrr_movements v WHERE v.merchant_id = OLD.merchant_id AND v.id = OLD.movement_id AND v.scheduled) THEN
        RETURN OLD;
    END IF;
    RAISE EXCEPTION '% is not permitted on immutable billing facts in %', TG_OP, TG_TABLE_NAME USING ERRCODE='23514';
END;
$$;
CREATE TRIGGER immutable_mrr_movements BEFORE UPDATE OR DELETE ON billing.mrr_movements
    FOR EACH ROW EXECUTE FUNCTION billing.guard_mrr_movement();
CREATE TRIGGER immutable_mrr_movement_lines BEFORE UPDATE OR DELETE ON billing.mrr_movement_lines
    FOR EACH ROW EXECUTE FUNCTION billing.guard_mrr_movement_line();

-- Why a subscription adding p_level is at risk, from its own status. Canceled
-- at its period end, it keeps the reasons it had until its MRR ends.
CREATE OR REPLACE FUNCTION billing.subscription_risk(s billing.subscriptions, p_level bigint, p_prev text[]) RETURNS text[]
LANGUAGE sql IMMUTABLE SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
SELECT CASE
    WHEN p_level = 0 OR s.deleted_at IS NOT NULL THEN '{}'::text[]
    WHEN s.status = 'canceled' THEN p_prev
    ELSE array_remove(ARRAY[
        CASE WHEN s.status = 'past_due' THEN 'past_due' END,
        CASE WHEN s.status = 'awaiting_method' THEN 'needs_payment_method' END,
        CASE WHEN s.status = 'unverified' THEN 'unverified' END]::text[], NULL)
END
$$;
COMMENT ON FUNCTION billing.subscription_risk(s billing.subscriptions, p_level bigint, p_prev text[]) IS 'The reasons a subscription adding MRR is at risk, from its status: past_due, needs_payment_method (awaiting_method; a provider''s past due is past_due) and unverified. Canceled at its period end, it keeps p_prev.';

-- The MRR writers read by key, planned for the table as it is, without
-- sequential scans: a write's work grows with what it changed.
CREATE OR REPLACE FUNCTION billing.refresh_subscription_mrr(s billing.subscriptions, p_at timestamptz, p_cause text) RETURNS void
LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' SET enable_seqscan TO off SET plan_cache_mode TO force_custom_plan AS $$
DECLARE
    v_price billing.prices;
    v_prev billing.subscription_mrr;
    v_level bigint := 0;
    v_live boolean := false;
    v_ends timestamptz;
    v_risk text[];
BEGIN
    SELECT * INTO v_price FROM billing.prices WHERE merchant_id = s.merchant_id AND id = s.price_id;
    IF NOT FOUND THEN
        RETURN;
    END IF;
    SELECT * INTO v_prev FROM billing.subscription_mrr WHERE merchant_id = s.merchant_id AND customer_id = s.customer_id AND subscription_id = s.id;
    IF s.deleted_at IS NULL THEN
        IF s.status = 'canceled' THEN
            -- A cancellation at period end contributes until it ends (an older
            -- one recorded no end); a subscription replaced by its upgrade
            -- leaves at once.
            v_ends := COALESCE(s.ended_at, s.current_period_ends_at);
            v_live := s.cancel_type IS DISTINCT FROM 'upgrade' AND COALESCE(v_ends > p_at, false);
        ELSE
            v_live := s.status IN ('active', 'past_due', 'awaiting_method', 'unverified');
        END IF;
        -- A trial contributes from its conversion: the first renewal of a
        -- price with a trial phase.
        IF v_live AND v_price.trial_duration_hours IS NOT NULL
           AND (s.current_period_starts_at IS NULL OR s.current_period_starts_at <= s.started_at) THEN
            v_live := false;
        END IF;
    END IF;
    IF v_live THEN
        v_level := billing.price_mrr(v_price, s.quantity);
    ELSE
        v_ends := NULL;
    END IF;
    v_risk := billing.subscription_risk(s, v_level, COALESCE(v_prev.risk, '{}'));
    IF v_prev.subscription_id IS NULL THEN
        INSERT INTO billing.subscription_mrr (merchant_id, subscription_id, customer_id, currency, product_id, price_id, mrr, contributes,
            ends_at, changed_at, cause, recorded_currency, recorded_product_id, recorded_price_id, recorded_mrr, recorded_ends_at,
            risk, recorded_risk, risk_changed_at)
        VALUES (s.merchant_id, s.id, s.customer_id, v_price.currency, s.product_id, s.price_id, v_level, v_live,
            v_ends, p_at, p_cause, v_price.currency, s.product_id, s.price_id, 0, NULL,
            v_risk, '{}', CASE WHEN v_risk <> '{}' THEN p_at END);
    ELSIF ROW(v_prev.currency, v_prev.product_id, v_prev.price_id, v_prev.mrr, v_prev.contributes, v_prev.ends_at, v_prev.risk)
          IS DISTINCT FROM ROW(v_price.currency, s.product_id, s.price_id, v_level, v_live, v_ends, v_risk) THEN
        UPDATE billing.subscription_mrr
           SET currency = v_price.currency, product_id = s.product_id, price_id = s.price_id, mrr = v_level, contributes = v_live,
               ends_at = v_ends, risk = v_risk,
               risk_changed_at = CASE WHEN v_prev.risk <> v_risk THEN greatest(p_at, v_prev.risk_changed_at) ELSE v_prev.risk_changed_at END,
               -- A change of what MRR counts, or of when it ends, is dated and
               -- explained by this one.
               changed_at = CASE WHEN ROW(v_prev.currency, v_prev.product_id, v_prev.price_id, v_prev.mrr, v_prev.ends_at)
                   IS DISTINCT FROM ROW(v_price.currency, s.product_id, s.price_id, v_level, v_ends) THEN p_at ELSE v_prev.changed_at END,
               cause = CASE WHEN ROW(v_prev.currency, v_prev.product_id, v_prev.price_id, v_prev.mrr, v_prev.ends_at)
                   IS DISTINCT FROM ROW(v_price.currency, s.product_id, s.price_id, v_level, v_ends) THEN p_cause ELSE v_prev.cause END
         WHERE merchant_id = s.merchant_id AND customer_id = s.customer_id AND subscription_id = s.id;
    END IF;
END;
$$;
COMMENT ON FUNCTION billing.refresh_subscription_mrr(s billing.subscriptions, p_at timestamptz, p_cause text) IS 'Recomputes one subscription''s subscription_mrr at p_at: its level, its end and its status reasons. A change makes the row dirty; billing.settle_mrr records it at commit.';

-- The lines a customer's changes in one currency make at p_at: each changed
-- subscription's MRR there before (as recorded) and after. A subscription
-- that moves to another product or currency leaves one and enters the other.
CREATE FUNCTION billing.pending_mrr_lines(p_merchant uuid, p_customer uuid, p_currency text, p_at timestamptz)
RETURNS TABLE(subscription_id uuid, product_id uuid, price_id uuid, mrr_before bigint, mrr_after bigint, cause text)
LANGUAGE sql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' SET enable_seqscan TO off SET plan_cache_mode TO force_custom_plan AS $$
SELECT m.subscription_id, x.product_id, x.price_id, x.mrr_before, x.mrr_after, m.cause
  FROM billing.subscription_mrr m
 CROSS JOIN LATERAL (SELECT
        CASE WHEN m.recorded_currency = p_currency AND (m.recorded_ends_at IS NULL OR m.recorded_ends_at > p_at) THEN m.recorded_mrr ELSE 0 END AS was,
        CASE WHEN m.currency = p_currency AND (m.ends_at IS NULL OR m.ends_at > p_at) THEN m.mrr ELSE 0 END AS now,
        (m.recorded_currency, m.recorded_product_id) = (m.currency, m.product_id) AS stays) e
 CROSS JOIN LATERAL (SELECT m.product_id, m.price_id, e.was, e.now WHERE e.stays
                     UNION ALL SELECT m.recorded_product_id, m.recorded_price_id, e.was, 0 WHERE NOT e.stays
                     UNION ALL SELECT m.product_id, m.price_id, 0, e.now WHERE NOT e.stays) x(product_id, price_id, mrr_before, mrr_after)
 WHERE m.merchant_id = p_merchant AND m.customer_id = p_customer AND m.dirty
   AND p_currency IN (m.currency, m.recorded_currency) AND x.mrr_before <> x.mrr_after
$$;

-- One customer's dirty rows: a movement per currency whose MRR changed, dated
-- at the latest change (never before a movement already recorded), with its
-- lines; then every end still ahead written again as a scheduled movement,
-- classified along the customer's timeline. Settlements of one customer take
-- turns on an advisory lock, which no other writer takes.
CREATE FUNCTION billing.settle_customer_mrr(p_merchant uuid, p_customer uuid) RETURNS void
LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' SET enable_seqscan TO off SET plan_cache_mode TO force_custom_plan AS $$
DECLARE
    v_at timestamptz;
    v_currency text;
    v_end record;
    v_total bigint;
    v_before bigint;
    v_after bigint;
    v_logo integer;
    v_active integer;
    v_first timestamptz;
    v_cause text;
    v_kind text;
    v_movement uuid;
BEGIN
    SELECT max(m.changed_at) INTO v_at FROM billing.subscription_mrr m
     WHERE m.merchant_id = p_merchant AND m.customer_id = p_customer AND m.dirty
       AND ROW(m.currency, m.product_id, m.price_id, m.mrr, m.ends_at)
           IS DISTINCT FROM ROW(m.recorded_currency, m.recorded_product_id, m.recorded_price_id, m.recorded_mrr, m.recorded_ends_at);
    IF v_at IS NULL THEN
        RETURN;
    END IF;
    PERFORM pg_advisory_xact_lock(hashtextextended('openrails.mrr:' || p_merchant::text || ':' || p_customer::text, 0));
    SELECT greatest(v_at, max(v.occurred_at)) INTO v_at FROM billing.mrr_movements v
     WHERE v.merchant_id = p_merchant AND v.customer_id = p_customer AND NOT v.scheduled;
    DELETE FROM billing.mrr_movement_lines l
     USING billing.mrr_movements v
     WHERE v.merchant_id = p_merchant AND v.customer_id = p_customer AND v.scheduled AND v.occurred_at > v_at
       AND l.merchant_id = p_merchant AND l.movement_id = v.id;
    DELETE FROM billing.mrr_movements v
     WHERE v.merchant_id = p_merchant AND v.customer_id = p_customer AND v.scheduled AND v.occurred_at > v_at;
    -- The customer's currencies with MRR at the change.
    SELECT count(*) FILTER (WHERE x.mrr_after > 0) INTO v_active
      FROM (SELECT DISTINCT ON (v.currency) v.mrr_after FROM billing.mrr_movements v
             WHERE v.merchant_id = p_merchant AND v.customer_id = p_customer AND v.occurred_at <= v_at
             ORDER BY v.currency, v.occurred_at DESC, v.id DESC) x;
    FOR v_currency IN
        SELECT DISTINCT c.currency FROM billing.subscription_mrr m
         CROSS JOIN LATERAL (VALUES (m.currency), (m.recorded_currency)) c(currency)
         WHERE m.merchant_id = p_merchant AND m.customer_id = p_customer AND m.dirty
         ORDER BY 1
    LOOP
        SELECT COALESCE(sum(l.mrr_after - l.mrr_before), 0) INTO v_total
          FROM billing.pending_mrr_lines(p_merchant, p_customer, v_currency, v_at) l;
        v_movement := NULL;
        IF v_total <> 0 THEN
            SELECT v.mrr_after, v.cohort_at INTO v_before, v_first FROM billing.mrr_movements v
             WHERE v.merchant_id = p_merchant AND v.customer_id = p_customer AND v.currency = v_currency AND v.occurred_at <= v_at
             ORDER BY v.occurred_at DESC, v.id DESC LIMIT 1;
            v_before := COALESCE(v_before, 0);
            v_after := v_before + v_total;
            v_logo := (v_after > 0)::integer - (v_before > 0)::integer;
            -- An upgrade explains the whole movement; otherwise its largest
            -- change does.
            SELECT p.cause INTO v_cause
              FROM (SELECT l.subscription_id, l.cause, sum(l.mrr_after - l.mrr_before) AS net
                      FROM billing.pending_mrr_lines(p_merchant, p_customer, v_currency, v_at) l
                     GROUP BY l.subscription_id, l.cause) p
             WHERE p.net <> 0 ORDER BY (p.cause = 'upgrade') DESC, abs(p.net) DESC, p.subscription_id LIMIT 1;
            v_kind := CASE
                WHEN v_cause IN ('prune', 'undo') THEN 'correction'
                WHEN v_cause = 'import' THEN CASE WHEN v_total > 0 THEN 'opening' ELSE 'correction' END
                WHEN v_before = 0 THEN CASE WHEN v_first IS NULL THEN 'new' ELSE 'reactivation' END
                WHEN v_after = 0 THEN 'churn'
                WHEN v_total > 0 THEN 'expansion'
                ELSE 'contraction'
            END;
            IF v_first IS NULL THEN
                v_first := v_at;
                -- An adopted subscription's cohort is when it started.
                IF v_kind = 'opening' THEN
                    SELECT least(v_at, min(s.started_at)) INTO v_first
                      FROM billing.pending_mrr_lines(p_merchant, p_customer, v_currency, v_at) l
                      JOIN billing.subscriptions s ON s.merchant_id = p_merchant AND s.id = l.subscription_id
                     WHERE l.mrr_after > 0;
                END IF;
            END IF;
            v_active := v_active + v_logo;
            INSERT INTO billing.mrr_movements (merchant_id, customer_id, currency, kind, delta, mrr_before, mrr_after, logo_delta,
                customer_logo_delta, cohort_at, cause, voluntary, occurred_at, scheduled)
            VALUES (p_merchant, p_customer, v_currency, v_kind, v_total, v_before, v_after, v_logo,
                (v_active > 0)::integer - (v_active - v_logo > 0)::integer, v_first, v_cause,
                CASE WHEN v_kind = 'churn' THEN v_cause IN ('canceled', 'merchant_canceled') END, v_at, false)
            RETURNING id INTO v_movement;
        END IF;
        -- A move between products at an equal MRR is lines alone.
        INSERT INTO billing.mrr_movement_lines (merchant_id, movement_id, subscription_id, product_id, price_id, delta, mrr_before, mrr_after,
            subscriber_delta, cause, occurred_at, currency)
        SELECT p_merchant, v_movement, l.subscription_id, l.product_id, l.price_id, l.mrr_after - l.mrr_before, l.mrr_before, l.mrr_after,
               (l.mrr_after > 0)::integer - (l.mrr_before > 0)::integer, l.cause, v_at, v_currency
          FROM billing.pending_mrr_lines(p_merchant, p_customer, v_currency, v_at) l;
    END LOOP;
    -- Each end still ahead, by when it comes: churn, or contraction when
    -- other MRR stays.
    FOR v_end IN
        SELECT m.currency, m.ends_at AS at, sum(m.mrr)::bigint AS level,
               (array_agg(billing.mrr_cancel_cause(s.cancel_type) ORDER BY m.mrr DESC, m.subscription_id))[1] AS cause
          FROM billing.subscription_mrr m
          JOIN billing.subscriptions s ON s.merchant_id = m.merchant_id AND s.id = m.subscription_id
         WHERE m.merchant_id = p_merchant AND m.customer_id = p_customer AND m.mrr > 0 AND m.ends_at > v_at
         GROUP BY m.currency, m.ends_at
         ORDER BY m.ends_at, m.currency
    LOOP
        SELECT v.mrr_after, v.cohort_at INTO v_before, v_first FROM billing.mrr_movements v
         WHERE v.merchant_id = p_merchant AND v.customer_id = p_customer AND v.currency = v_end.currency
         ORDER BY v.occurred_at DESC, v.id DESC LIMIT 1;
        v_before := COALESCE(v_before, 0);
        v_after := v_before - v_end.level;
        v_logo := -(v_after = 0)::integer;
        v_kind := CASE WHEN v_after = 0 THEN 'churn' ELSE 'contraction' END;
        v_active := v_active + v_logo;
        INSERT INTO billing.mrr_movements (merchant_id, customer_id, currency, kind, delta, mrr_before, mrr_after, logo_delta,
            customer_logo_delta, cohort_at, cause, voluntary, occurred_at, scheduled)
        VALUES (p_merchant, p_customer, v_end.currency, v_kind, -v_end.level, v_before, v_after, v_logo,
            (v_active > 0)::integer - (v_active - v_logo > 0)::integer, v_first, v_end.cause,
            CASE WHEN v_kind = 'churn' THEN v_end.cause IN ('canceled', 'merchant_canceled') END, v_end.at, true)
        RETURNING id INTO v_movement;
        INSERT INTO billing.mrr_movement_lines (merchant_id, movement_id, subscription_id, product_id, price_id, delta, mrr_before, mrr_after,
            subscriber_delta, cause, occurred_at, currency)
        SELECT p_merchant, v_movement, m.subscription_id, m.product_id, m.price_id, -m.mrr, m.mrr, 0, -1,
               billing.mrr_cancel_cause(s.cancel_type), v_end.at, v_end.currency
          FROM billing.subscription_mrr m
          JOIN billing.subscriptions s ON s.merchant_id = m.merchant_id AND s.id = m.subscription_id
         WHERE m.merchant_id = p_merchant AND m.customer_id = p_customer AND m.currency = v_end.currency
           AND m.ends_at = v_end.at AND m.mrr > 0;
    END LOOP;
END;
$$;
COMMENT ON FUNCTION billing.settle_customer_mrr(p_merchant uuid, p_customer uuid) IS 'Records one customer''s dirty subscription_mrr rows: per currency one mrr_movements row at the latest change, classified by the customer''s MRR there, with its lines (lines alone when products moved at an equal MRR); then deletes its scheduled movements after the change and writes one per currency and end still ahead.';

CREATE OR REPLACE FUNCTION billing.settle_mrr(p_merchant uuid) RETURNS void
LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' SET enable_seqscan TO off SET plan_cache_mode TO force_custom_plan AS $$
DECLARE
    v_customer uuid;
    v_row record;
    v_case billing.mrr_risk_cases;
    v_at timestamptz;
    v_moved boolean;
    v_outcome text;
BEGIN
    FOR v_customer IN
        SELECT DISTINCT m.customer_id FROM billing.subscription_mrr m
         WHERE m.merchant_id = p_merchant AND m.dirty ORDER BY m.customer_id
    LOOP
        PERFORM billing.settle_customer_mrr(p_merchant, v_customer);
    END LOOP;
    -- At-risk MRR: a subscription whose reasons changed, or whose MRR, end or
    -- currency changed while at risk, opens, chains, splits or closes its
    -- case and episodes. Each case has an episode of reason any.
    FOR v_row IN
        SELECT m.*, s.status, s.cancel_type, s.deleted_at
          FROM billing.subscription_mrr m
          JOIN billing.subscriptions s ON s.merchant_id = m.merchant_id AND s.id = m.subscription_id
         WHERE m.merchant_id = p_merchant AND m.dirty
           AND (m.risk <> m.recorded_risk OR m.recorded_risk <> '{}'
                AND ROW(m.currency, m.mrr, m.ends_at) IS DISTINCT FROM ROW(m.recorded_currency, m.recorded_mrr, m.recorded_ends_at))
         ORDER BY m.subscription_id
    LOOP
        v_moved := v_row.recorded_risk <> '{}' AND v_row.currency <> v_row.recorded_currency;
        -- Dated at the change of its reasons, else of its MRR.
        v_at := CASE WHEN v_row.risk <> v_row.recorded_risk THEN v_row.risk_changed_at ELSE v_row.changed_at END;
        -- Its case: open, or closed at an end still ahead, which a change
        -- before that end reopens.
        SELECT * INTO v_case FROM billing.mrr_risk_cases
         WHERE merchant_id = p_merchant AND subscription_id = v_row.subscription_id AND (closed_at IS NULL OR closed_at > v_at)
         ORDER BY opened_at DESC LIMIT 1;
        IF v_case.closed_at IS NOT NULL THEN
            UPDATE billing.mrr_risk_episodes SET left_at = NULL, mrr_at_exit = NULL
             WHERE merchant_id = p_merchant AND case_id = v_case.id AND left_at = v_case.closed_at;
            UPDATE billing.mrr_risk_cases SET closed_at = NULL, outcome = NULL, mrr_at_exit = NULL, voluntary = NULL
             WHERE merchant_id = p_merchant AND id = v_case.id;
        END IF;
        IF v_case.id IS NOT NULL THEN
            UPDATE billing.mrr_risk_episodes SET left_at = greatest(v_at, entered_at), mrr_at_exit = v_row.recorded_mrr
             WHERE merchant_id = p_merchant AND case_id = v_case.id AND left_at IS NULL AND reason <> 'any'
               AND (v_moved OR reason <> ALL(v_row.risk));
            IF v_moved OR v_row.risk = '{}' THEN
                -- Healthy again while adding MRR is recovered; MRR that ended
                -- churned; a prune, an upgrade or a new currency supersedes.
                v_outcome := CASE
                    WHEN v_moved OR v_row.deleted_at IS NOT NULL OR v_row.cancel_type = 'upgrade' THEN 'superseded'
                    WHEN v_row.mrr > 0 THEN 'recovered'
                    ELSE 'churned' END;
                UPDATE billing.mrr_risk_episodes SET left_at = greatest(v_at, entered_at), mrr_at_exit = v_row.recorded_mrr
                 WHERE merchant_id = p_merchant AND case_id = v_case.id AND left_at IS NULL;
                UPDATE billing.mrr_risk_cases
                   SET closed_at = greatest(v_at, opened_at), outcome = v_outcome,
                       mrr_at_exit = CASE WHEN v_outcome = 'recovered' THEN v_row.mrr ELSE v_row.recorded_mrr END,
                       voluntary = CASE WHEN v_outcome = 'churned' THEN COALESCE(v_row.cancel_type IN ('user', 'merchant'), false) END
                 WHERE merchant_id = p_merchant AND id = v_case.id;
                v_case := NULL;
            ELSIF v_row.mrr <> v_row.recorded_mrr THEN
                -- Its MRR changed while at risk: what stays at risk stays at
                -- the new amount.
                UPDATE billing.mrr_risk_episodes SET left_at = greatest(v_at, entered_at), mrr_at_exit = v_row.recorded_mrr
                 WHERE merchant_id = p_merchant AND case_id = v_case.id AND left_at IS NULL;
            END IF;
        END IF;
        IF v_row.risk <> '{}' THEN
            -- A case opens with the subscription's first reason.
            IF v_case.id IS NULL THEN
                INSERT INTO billing.mrr_risk_cases (merchant_id, subscription_id, customer_id, currency, entry_reason, opened_at, mrr_at_entry)
                VALUES (p_merchant, v_row.subscription_id, v_row.customer_id, v_row.currency, v_row.risk[1], v_at, v_row.mrr)
                RETURNING * INTO v_case;
            END IF;
            INSERT INTO billing.mrr_risk_episodes (merchant_id, case_id, subscription_id, currency, reason, entered_at, mrr_at_entry)
            SELECT p_merchant, v_case.id, v_row.subscription_id, v_row.currency, r.reason, v_at, v_row.mrr
              FROM unnest(v_row.risk || '{any}'::text[]) AS r(reason)
             WHERE NOT EXISTS (SELECT 1 FROM billing.mrr_risk_episodes e
                                WHERE e.merchant_id = p_merchant AND e.subscription_id = v_row.subscription_id AND e.reason = r.reason AND e.left_at IS NULL);
            -- Canceled at its period end, its MRR ends there at risk: the
            -- case closes churned at the end, and a change before it reopens.
            IF v_row.ends_at > v_at THEN
                UPDATE billing.mrr_risk_episodes SET left_at = v_row.ends_at, mrr_at_exit = v_row.mrr
                 WHERE merchant_id = p_merchant AND case_id = v_case.id AND left_at IS NULL;
                UPDATE billing.mrr_risk_cases
                   SET closed_at = v_row.ends_at, outcome = 'churned', mrr_at_exit = v_row.mrr,
                       voluntary = COALESCE(v_row.cancel_type IN ('user', 'merchant'), false)
                 WHERE merchant_id = p_merchant AND id = v_case.id;
            END IF;
        END IF;
    END LOOP;
    UPDATE billing.subscription_mrr
       SET recorded_currency = currency, recorded_product_id = product_id, recorded_price_id = price_id, recorded_mrr = mrr,
           recorded_ends_at = ends_at, recorded_risk = risk
     WHERE merchant_id = p_merchant AND dirty;
END;
$$;
COMMENT ON FUNCTION billing.settle_mrr(p_merchant uuid) IS 'Records the merchant''s dirty subscription_mrr rows: each customer''s movements (billing.settle_customer_mrr), then each subscription''s risk case and episodes.';

CREATE OR REPLACE FUNCTION billing.subscriptions_track_mrr() RETURNS trigger
LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
BEGIN
    IF NEW.price_id IS NULL OR TG_OP = 'UPDATE' AND ROW(OLD.status, OLD.price_id, OLD.product_id, OLD.quantity, OLD.ended_at,
            OLD.current_period_starts_at, OLD.current_period_ends_at, OLD.deleted_at, OLD.cancel_type)
        IS NOT DISTINCT FROM ROW(NEW.status, NEW.price_id, NEW.product_id, NEW.quantity, NEW.ended_at,
            NEW.current_period_starts_at, NEW.current_period_ends_at, NEW.deleted_at, NEW.cancel_type) THEN
        RETURN NULL;
    END IF;
    IF billing.billing_restore_active(NEW.merchant_id) THEN
        RETURN NULL;
    END IF;
    PERFORM billing.refresh_subscription_mrr(NEW, NEW.updated_at, CASE
        WHEN current_setting('openrails.mrr_cause', true) IN ('import', 'undo') THEN current_setting('openrails.mrr_cause', true)
        WHEN TG_OP = 'UPDATE' AND OLD.deleted_at IS DISTINCT FROM NEW.deleted_at THEN CASE WHEN NEW.deleted_at IS NULL THEN 'undo' ELSE 'prune' END
        WHEN NEW.status = 'canceled' THEN billing.mrr_cancel_cause(NEW.cancel_type)
        WHEN TG_OP = 'INSERT' THEN 'subscribed'
        WHEN OLD.price_id IS DISTINCT FROM NEW.price_id THEN 'price_change'
        WHEN OLD.quantity IS DISTINCT FROM NEW.quantity THEN 'seat_change'
        WHEN OLD.status = 'canceled' THEN 'resumed'
        WHEN OLD.status = 'pending' THEN 'subscribed'
        ELSE 'renewed'
    END);
    RETURN NULL;
END;
$$;
DROP TRIGGER trg_subscriptions_track_mrr ON billing.subscriptions;
CREATE TRIGGER trg_subscriptions_track_mrr AFTER INSERT OR UPDATE OF status, price_id, product_id, quantity, ended_at, current_period_starts_at, current_period_ends_at, deleted_at, cancel_type ON billing.subscriptions
    FOR EACH ROW EXECUTE FUNCTION billing.subscriptions_track_mrr();

CREATE CONSTRAINT TRIGGER subscription_mrr_settle AFTER INSERT OR UPDATE ON billing.subscription_mrr DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION billing.settle_at_commit();

-- What is at risk now, from state: each subscription adding MRR at p_at,
-- with its status reasons, card_expiring (the card it charges expires by its
-- period end: an engine subscription's own card or its customer's default, an
-- NMI schedule's only when the subscription names it) and delinquent (its
-- customer, in its currency); since is when they began as far as state tells:
-- the delinquency's or the current period's start, else when its status
-- reasons last changed. Inlined into its callers: no SET clause.
CREATE FUNCTION billing.mrr_risk_now(p_merchant uuid, p_at timestamptz)
RETURNS TABLE(merchant_id uuid, subscription_id uuid, customer_id uuid, currency text, mrr bigint, reasons text[], since timestamptz)
LANGUAGE sql STABLE AS $$
SELECT m.merchant_id, m.subscription_id, m.customer_id, m.currency, m.mrr, r.reasons,
       COALESCE(least(CASE WHEN c.expiring THEN s.current_period_starts_at END, d.entered_at), m.risk_changed_at, s.started_at)
  FROM billing.subscription_mrr m
  JOIN billing.subscriptions s ON s.merchant_id = m.merchant_id AND s.id = m.subscription_id
  LEFT JOIN billing.money_settings ms ON ms.merchant_id = s.merchant_id AND ms.customer_id = s.customer_id AND ms.currency = m.currency
   AND s.collection_policy = 'engine' AND s.payment_method_id IS NULL AND s.rail IN ('nmi', 'stripe')
  LEFT JOIN billing.payment_methods pm ON pm.merchant_id = s.merchant_id
   AND pm.id = CASE WHEN s.collection_policy IN ('engine', 'nmi_schedule') THEN COALESCE(s.payment_method_id, ms.default_payment_method_id) END
  LEFT JOIN billing.customer_delinquency d ON d.merchant_id = m.merchant_id AND d.customer_id = m.customer_id AND d.currency = m.currency
   AND d.state = 'delinquent'
 CROSS JOIN LATERAL (SELECT (make_date(pm.card_exp_year, pm.card_exp_month, 1) + interval '1 month') AT TIME ZONE 'UTC' <= s.current_period_ends_at AS expiring) c
 CROSS JOIN LATERAL (SELECT m.risk || CASE WHEN c.expiring THEN '{card_expiring}'::text[] ELSE '{}'::text[] END
                            || CASE WHEN d.customer_id IS NOT NULL THEN '{delinquent}'::text[] ELSE '{}'::text[] END AS reasons) r
 WHERE m.merchant_id = p_merchant AND m.mrr > 0 AND (m.ends_at IS NULL OR m.ends_at > p_at) AND s.deleted_at IS NULL
   AND r.reasons <> '{}'
$$;
COMMENT ON FUNCTION billing.mrr_risk_now(p_merchant uuid, p_at timestamptz) IS 'The merchant''s subscriptions whose MRR is at risk at p_at, from state: status reasons (subscription_mrr.risk), card_expiring and delinquent; since is when the state reasons began (the delinquency''s entered_at, the current period''s start for the card), else when the status reasons last changed.';

-- The scheduled changes a renewal will bill, priced now: the target's MRR
-- less the subscription's, from the renewal on or after the change's date.
-- Inlined into its callers: no SET clause.
CREATE FUNCTION billing.mrr_scheduled_changes(p_merchant uuid)
RETURNS TABLE(subscription_id uuid, customer_id uuid, currency text, effective_at timestamptz, amount bigint)
LANGUAGE sql STABLE AS $$
SELECT m.subscription_id, m.customer_id, m.currency, greatest(s.current_period_ends_at, c.effective_at), x.amount
  FROM billing.scheduled_changes c
  JOIN billing.subscription_mrr m ON m.merchant_id = c.merchant_id AND m.subscription_id = c.subscription_id
  JOIN billing.subscriptions s ON s.merchant_id = c.merchant_id AND s.id = c.subscription_id
  JOIN billing.prices p ON p.merchant_id = c.merchant_id AND p.id = c.price_id
 CROSS JOIN LATERAL (SELECT billing.price_mrr(p, COALESCE(c.quantity, s.quantity)) - m.mrr AS amount) x
 WHERE c.merchant_id = p_merchant AND c.status = 'scheduled' AND m.mrr > 0 AND m.ends_at IS NULL
   AND p.currency = m.currency AND x.amount <> 0
$$;
COMMENT ON FUNCTION billing.mrr_scheduled_changes(p_merchant uuid) IS 'The merchant''s scheduled changes that move MRR, priced at the read: amount is the target price''s MRR for its seats less the subscription''s, effective at the renewal on or after the change''s date.';

COMMENT ON TABLE billing.subscription_mrr IS 'What each subscription adds to MRR, kept by trg_subscriptions_track_mrr for every writer: its price''s MRR for its seats (billing.price_mrr), while it is live or canceled with its end ahead; a pending subscription, a trial and an ended one add 0. A cancellation at period end keeps its level and sets ends_at: its MRR is the level until then. The recorded_ columns are what the movements last counted; a row that differs (dirty) is settled at commit.';
COMMENT ON COLUMN billing.subscription_mrr.ends_at IS 'When a cancellation at period end takes the subscription out of MRR; NULL when none is scheduled. Nothing runs then: the scheduled movement written at the cancel counts from that instant.';
COMMENT ON COLUMN billing.subscription_mrr.recorded_ends_at IS 'The end the movements last counted.';
COMMENT ON COLUMN billing.subscription_mrr.changed_at IS 'The business instant of the change not yet recorded, or of the last one recorded: the writer''s updated_at.';
COMMENT ON COLUMN billing.subscription_mrr.risk IS 'Why the subscription''s MRR is at risk now from its status: past_due, needs_payment_method and unverified, in that order; empty when it is not at risk or adds no MRR. Canceled at its period end, it keeps them until its MRR ends. card_expiring and delinquent are read from state (billing.mrr_risk_now).';
COMMENT ON TABLE billing.mrr_movements IS 'One change of a customer''s MRR in one currency: new (first MRR), reactivation (MRR again after none), expansion, contraction, churn (to none; voluntary for a customer''s or merchant''s cancel), opening (an import adopting subscriptions) or correction (a prune or an undo). Written at the commit of the transaction that made it; a scheduled one (a cancellation at period end) is dated at the end and counts once that instant passes. Retention: permanent, never pruned.';
COMMENT ON COLUMN billing.mrr_movements.scheduled IS 'Dated at a period end decided earlier: a later settlement of the customer deletes and writes it again while its date is ahead.';
COMMENT ON COLUMN billing.mrr_movements.customer_logo_delta IS '1 when the customer gains MRR in a first currency, -1 when it loses it in its last, 0 otherwise: distinct customers.';
COMMENT ON COLUMN billing.mrr_movements.cohort_at IS 'When the customer first had MRR in the currency: its cohort.';
COMMENT ON COLUMN billing.mrr_movements.occurred_at IS 'The business instant of the change: its writer''s, or the end a cancellation at period end runs to.';
COMMENT ON TABLE billing.mrr_movement_lines IS 'Each subscription''s part of a movement, at its instant and in its currency: its product and price, its change of MRR and whether it began or stopped adding to it. A move to another product or currency leaves one and enters the other; at an equal MRR the lines stand alone (movement_id NULL). Retention: permanent, never pruned.';
COMMENT ON TABLE billing.mrr_risk_cases IS 'One stretch of a subscription''s MRR at risk for its status, from its first reason to its last: opened by its entry_reason, chaining reasons (mrr_risk_episodes), and closed recovered (healthy again while adding MRR), churned (its MRR ended while the case was open; voluntary for a customer''s or merchant''s cancel) or superseded (a prune, an upgrade or a change of currency). A cancel at period end closes it churned at the end, written at the cancel; a resume before then reopens it. card_expiring and delinquent cases end at their migration to state reads. The recovery rate is recovered MRR over recovered and churned. Retention: permanent, never pruned.';
COMMENT ON TABLE billing.mrr_risk_episodes IS 'One reason a subscription''s MRR was at risk, from when it entered to when it left, at one amount: a change of the subscription''s MRR while at risk ends its episodes and begins them again at the new amount. Every case has an episode of reason any, its MRR at risk whatever its reasons. Retention: permanent, never pruned.';
