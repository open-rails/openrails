-- parent: 3 sha256:cffc4a05bbf5e1f752f8de27f22c9419a334c4eb5993a1ce820321c796d2a530
-- Provider-neutral, read-only evidence retained by provider pulls.  These
-- rows are analysis evidence, not billing mirror rows and never authorize a
-- provider mutation.

SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '5min';

CREATE TABLE openrails.provider_evidence_snapshots (
    id uuid DEFAULT uuidv7() PRIMARY KEY,
    merchant_id uuid NOT NULL,
    reconciliation_run_id uuid NOT NULL,
    provider text NOT NULL,
    psp_id uuid NOT NULL,
    fetched_at timestamptz NOT NULL,
    window_since timestamptz,
    window_until timestamptz,
    capabilities jsonb NOT NULL DEFAULT '{}'::jsonb,
    coverage jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT provider_evidence_snapshots_provider_nonempty CHECK (btrim(provider) <> ''),
    CONSTRAINT provider_evidence_snapshots_capabilities_object CHECK (jsonb_typeof(capabilities) = 'object'),
    CONSTRAINT provider_evidence_snapshots_coverage_object CHECK (jsonb_typeof(coverage) = 'object'),
    CONSTRAINT provider_evidence_snapshots_window_order CHECK (window_since IS NULL OR window_until IS NULL OR window_until >= window_since)
);

ALTER TABLE ONLY openrails.provider_evidence_snapshots
    ADD CONSTRAINT provider_evidence_snapshots_merchant_fk FOREIGN KEY (merchant_id) REFERENCES openrails.merchants(id) ON DELETE CASCADE;
ALTER TABLE ONLY openrails.provider_evidence_snapshots
    ADD CONSTRAINT provider_evidence_snapshots_merchant_id_key UNIQUE (merchant_id, id);
ALTER TABLE ONLY openrails.provider_evidence_snapshots
    ADD CONSTRAINT provider_evidence_snapshots_psp_fk FOREIGN KEY (merchant_id, psp_id) REFERENCES openrails.psps(merchant_id, id) ON DELETE CASCADE;
CREATE UNIQUE INDEX uq_provider_evidence_snapshots_run ON openrails.provider_evidence_snapshots (merchant_id, reconciliation_run_id, provider, psp_id);
CREATE INDEX idx_provider_evidence_snapshots_merchant_time ON openrails.provider_evidence_snapshots (merchant_id, provider, psp_id, fetched_at DESC);

COMMENT ON TABLE openrails.provider_evidence_snapshots IS 'Immutable provider pull observations retained for provider-neutral billing analysis. Pulls are read-only and do not mutate external rails.';
COMMENT ON COLUMN openrails.provider_evidence_snapshots.reconciliation_run_id IS 'The observation run that produced this snapshot; retained for reproducibility and idempotent replays.';
COMMENT ON COLUMN openrails.provider_evidence_snapshots.coverage IS 'Normalized provider coverage proof from reconcile.RemoteSnapshot.Coverage.';

CREATE TABLE openrails.provider_evidence_transactions (
    id uuid DEFAULT uuidv7() PRIMARY KEY,
    merchant_id uuid NOT NULL,
    psp_id uuid NOT NULL,
    provider text NOT NULL,
    event_key text NOT NULL,
    transaction_id text NOT NULL DEFAULT '',
    subscription_ref text NOT NULL DEFAULT '',
    type text NOT NULL,
    success boolean NOT NULL,
    amount_cents bigint NOT NULL DEFAULT 0,
    currency text NOT NULL DEFAULT 'UNK',
    occurred_at timestamptz NOT NULL,
    source text NOT NULL DEFAULT '',
    customer_ref text NOT NULL DEFAULT '',
    customer_email text NOT NULL DEFAULT '',
    order_ref text NOT NULL DEFAULT '',
    decline_code text NOT NULL DEFAULT '',
    decline_reason text NOT NULL DEFAULT '',
    raw jsonb NOT NULL DEFAULT '{}'::jsonb,
    first_snapshot_id uuid NOT NULL,
    last_snapshot_id uuid NOT NULL,
    first_seen_at timestamptz NOT NULL,
    last_seen_at timestamptz NOT NULL,
    CONSTRAINT provider_evidence_transactions_provider_nonempty CHECK (btrim(provider) <> ''),
    CONSTRAINT provider_evidence_transactions_event_key_nonempty CHECK (btrim(event_key) <> ''),
    CONSTRAINT provider_evidence_transactions_type_nonempty CHECK (btrim(type) <> ''),
    CONSTRAINT provider_evidence_transactions_currency_shape CHECK (currency ~ '^[A-Z0-9]{3,12}$'),
    CONSTRAINT provider_evidence_transactions_raw_object CHECK (jsonb_typeof(raw) IN ('object','array'))
);

ALTER TABLE ONLY openrails.provider_evidence_transactions
    ADD CONSTRAINT provider_evidence_transactions_merchant_fk FOREIGN KEY (merchant_id) REFERENCES openrails.merchants(id) ON DELETE CASCADE;
ALTER TABLE ONLY openrails.provider_evidence_transactions
    ADD CONSTRAINT provider_evidence_transactions_psp_fk FOREIGN KEY (merchant_id, psp_id) REFERENCES openrails.psps(merchant_id, id) ON DELETE CASCADE;
ALTER TABLE ONLY openrails.provider_evidence_transactions
    ADD CONSTRAINT provider_evidence_transactions_first_snapshot_fk FOREIGN KEY (merchant_id, first_snapshot_id) REFERENCES openrails.provider_evidence_snapshots(merchant_id, id) ON DELETE CASCADE;
ALTER TABLE ONLY openrails.provider_evidence_transactions
    ADD CONSTRAINT provider_evidence_transactions_last_snapshot_fk FOREIGN KEY (merchant_id, last_snapshot_id) REFERENCES openrails.provider_evidence_snapshots(merchant_id, id) ON DELETE CASCADE;
CREATE UNIQUE INDEX uq_provider_evidence_transactions_event ON openrails.provider_evidence_transactions (merchant_id, psp_id, provider, event_key);
CREATE INDEX idx_provider_evidence_transactions_day ON openrails.provider_evidence_transactions (merchant_id, psp_id, provider, occurred_at, success);
CREATE INDEX idx_provider_evidence_transactions_subscription ON openrails.provider_evidence_transactions (merchant_id, psp_id, provider, subscription_ref, occurred_at);

COMMENT ON TABLE openrails.provider_evidence_transactions IS 'Canonical provider transaction/action evidence. Repeated pulls upsert the same event_key and advance last_seen_at, preventing duplicate report counts.';
COMMENT ON COLUMN openrails.provider_evidence_transactions.source IS 'Provider-declared event source (for example recurring or api); empty means the provider did not expose one.';
COMMENT ON COLUMN openrails.provider_evidence_transactions.raw IS 'Provider record preserved verbatim as normalized JSON for forensics and future classification.';

CREATE TABLE openrails.provider_evidence_subscriptions (
    id uuid DEFAULT uuidv7() PRIMARY KEY,
    snapshot_id uuid NOT NULL,
    merchant_id uuid NOT NULL,
    psp_id uuid NOT NULL,
    provider text NOT NULL,
    record_key text NOT NULL,
    provider_subscription_ref text NOT NULL DEFAULT '',
    status text NOT NULL DEFAULT '',
    raw_status text NOT NULL DEFAULT '',
    customer_ref text NOT NULL DEFAULT '',
    customer_email text NOT NULL DEFAULT '',
    username text NOT NULL DEFAULT '',
    plan_ref text NOT NULL DEFAULT '',
    next_billing_at timestamptz,
    last_billed_at timestamptz,
    amount_cents bigint NOT NULL DEFAULT 0,
    currency text NOT NULL DEFAULT 'UNK',
    raw jsonb NOT NULL DEFAULT '{}'::jsonb,
    CONSTRAINT provider_evidence_subscriptions_provider_nonempty CHECK (btrim(provider) <> ''),
    CONSTRAINT provider_evidence_subscriptions_record_key_nonempty CHECK (btrim(record_key) <> ''),
    CONSTRAINT provider_evidence_subscriptions_currency_shape CHECK (currency ~ '^[A-Z0-9]{3,12}$'),
    CONSTRAINT provider_evidence_subscriptions_raw_object CHECK (jsonb_typeof(raw) IN ('object','array'))
);

ALTER TABLE ONLY openrails.provider_evidence_subscriptions
    ADD CONSTRAINT provider_evidence_subscriptions_snapshot_fk FOREIGN KEY (merchant_id, snapshot_id) REFERENCES openrails.provider_evidence_snapshots(merchant_id, id) ON DELETE CASCADE;
ALTER TABLE ONLY openrails.provider_evidence_subscriptions
    ADD CONSTRAINT provider_evidence_subscriptions_merchant_fk FOREIGN KEY (merchant_id) REFERENCES openrails.merchants(id) ON DELETE CASCADE;
ALTER TABLE ONLY openrails.provider_evidence_subscriptions
    ADD CONSTRAINT provider_evidence_subscriptions_psp_fk FOREIGN KEY (merchant_id, psp_id) REFERENCES openrails.psps(merchant_id, id) ON DELETE CASCADE;
CREATE UNIQUE INDEX uq_provider_evidence_subscriptions_record ON openrails.provider_evidence_subscriptions (merchant_id, snapshot_id, record_key);
CREATE INDEX idx_provider_evidence_subscriptions_ref ON openrails.provider_evidence_subscriptions (merchant_id, psp_id, provider, provider_subscription_ref);
CREATE INDEX idx_provider_evidence_subscriptions_due ON openrails.provider_evidence_subscriptions (merchant_id, psp_id, next_billing_at);

COMMENT ON TABLE openrails.provider_evidence_subscriptions IS 'Point-in-time provider subscription roster observations. A new snapshot preserves status and next-billing history without changing OpenRails subscriptions.';

CREATE TABLE openrails.provider_evidence_payment_methods (
    id uuid DEFAULT uuidv7() PRIMARY KEY,
    snapshot_id uuid NOT NULL,
    merchant_id uuid NOT NULL,
    psp_id uuid NOT NULL,
    provider text NOT NULL,
    record_key text NOT NULL,
    customer_ref text NOT NULL DEFAULT '',
    card_last4 text NOT NULL DEFAULT '',
    card_expiry text NOT NULL DEFAULT '',
    customer_email text NOT NULL DEFAULT '',
    raw jsonb NOT NULL DEFAULT '{}'::jsonb,
    CONSTRAINT provider_evidence_payment_methods_provider_nonempty CHECK (btrim(provider) <> ''),
    CONSTRAINT provider_evidence_payment_methods_record_key_nonempty CHECK (btrim(record_key) <> ''),
    CONSTRAINT provider_evidence_payment_methods_raw_object CHECK (jsonb_typeof(raw) IN ('object','array'))
);

ALTER TABLE ONLY openrails.provider_evidence_payment_methods
    ADD CONSTRAINT provider_evidence_payment_methods_snapshot_fk FOREIGN KEY (merchant_id, snapshot_id) REFERENCES openrails.provider_evidence_snapshots(merchant_id, id) ON DELETE CASCADE;
ALTER TABLE ONLY openrails.provider_evidence_payment_methods
    ADD CONSTRAINT provider_evidence_payment_methods_merchant_fk FOREIGN KEY (merchant_id) REFERENCES openrails.merchants(id) ON DELETE CASCADE;
ALTER TABLE ONLY openrails.provider_evidence_payment_methods
    ADD CONSTRAINT provider_evidence_payment_methods_psp_fk FOREIGN KEY (merchant_id, psp_id) REFERENCES openrails.psps(merchant_id, id) ON DELETE CASCADE;
CREATE UNIQUE INDEX uq_provider_evidence_payment_methods_record ON openrails.provider_evidence_payment_methods (merchant_id, snapshot_id, record_key);
CREATE INDEX idx_provider_evidence_payment_methods_customer ON openrails.provider_evidence_payment_methods (merchant_id, psp_id, customer_ref);

COMMENT ON TABLE openrails.provider_evidence_payment_methods IS 'Point-in-time provider payment-method roster observations retained for identity joins and forensics.';

-- Provider pulls run through the unprivileged runtime role. Evidence is
-- merchant-scoped by every query and is never a billing mirror write. Some
-- migration-only test databases do not provision that runtime role, so keep
-- the grant conditional while applying it in deployed databases.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'openrails_app') THEN
        GRANT SELECT, INSERT, UPDATE ON TABLE openrails.provider_evidence_snapshots TO openrails_app;
        GRANT SELECT, INSERT, UPDATE ON TABLE openrails.provider_evidence_transactions TO openrails_app;
        GRANT SELECT, INSERT, UPDATE ON TABLE openrails.provider_evidence_subscriptions TO openrails_app;
        GRANT SELECT, INSERT, UPDATE ON TABLE openrails.provider_evidence_payment_methods TO openrails_app;
    END IF;
END;
$$;
CREATE OR REPLACE FUNCTION openrails.guard_billing_restore_receipt() RETURNS trigger
    LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'openrails', 'pg_temp' AS $$
DECLARE item record; occupied boolean;
BEGIN
    IF TG_OP='DELETE' THEN
        IF OLD.kind='billing_restore' THEN
            RAISE EXCEPTION 'billing restore receipts are immutable' USING ERRCODE='23514';
        END IF;
        RETURN OLD;
    END IF;
    IF NEW.kind<>'billing_restore' AND (TG_OP='INSERT' OR OLD.kind<>'billing_restore') THEN RETURN NEW; END IF;
    IF NEW.merchant_id IS DISTINCT FROM openrails.current_merchant_id() THEN
        RAISE EXCEPTION 'billing restore merchant mismatch' USING ERRCODE='42501';
    END IF;
    -- The merchant row is the serialization point used by begin_billing_restore
    -- and by FK-backed first writes. No database-owner exemption or GUC-only
    -- permission can create a receipt for an occupied destination.
    PERFORM 1 FROM openrails.merchants WHERE id=NEW.merchant_id AND status='active' AND deleted_at IS NULL FOR UPDATE;
    IF NOT FOUND THEN RAISE EXCEPTION 'billing restore merchant missing or inactive' USING ERRCODE='P0002'; END IF;
    IF TG_OP='INSERT' THEN
        IF NEW.status<>'running' OR NEW.finished_at IS NOT NULL OR NEW.summary IS NOT NULL THEN
            RAISE EXCEPTION 'billing restore receipts must begin running and unfinished' USING ERRCODE='23514';
        END IF;
        FOR item IN SELECT c.relname FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
            JOIN pg_attribute a ON a.attrelid=c.oid AND a.attname='merchant_id' AND NOT a.attisdropped
            WHERE n.nspname=TG_TABLE_SCHEMA AND c.relkind IN ('r','p')
              AND c.relname = ANY(ARRAY[
                  'account_updater_batches',
                  'admission_denials_hourly',
                  'admission_operations',
                  'billing_policies',
                  'billing_policy_bindings',
                  'catalog_applications',
                  'catalog_meters',
                  'catalog_rate_cards',
                  'catalogs',
                  'checkout_sessions',
                  'credential_publications',
                  'custodians',
                  'custody_migrations',
                  'customer_delinquency',
                  'customer_invoice_profiles',
                  'customers',
                  'dashboard_configs',
                  'destructive_action_switch',
                  'destructive_run_before_images',
                  'entitlements',
                  'grants',
                  'host_outbox',
                  'invoice_items',
                  'invoice_payments',
                  'invoices',
                  'invoker_spend_limits',
                  'ledger_accounts',
                  'ledger_transfers',
                  'maintenance_runs',
                  'merchant_configurations',
                  'merchant_configuration_applications',
                  'merchant_deks',
                  'merchant_destructive_policy',
                  'merchant_secrets',
                  'merchant_webhooks',
                  'merchants',
                  'metered_rating_watermarks',
                  'money_settings',
                  'notifications',
                  'operation_authorizations',
                  'payment_methods',
                  'payments',
                  'price_key_movements',
                  'price_psp_bindings',
                  'prices',
                  'product_archive_operations',
                  'products',
                  'provider_billing_observations',
                  'provider_billing_qualifications',
                  'provider_evidence_payment_methods',
                  'provider_evidence_snapshots',
                  'provider_evidence_subscriptions',
                  'provider_evidence_transactions',
                  'psps',
                  'rail_customer_accounts',
                  'rail_intents',
                  'rail_mutation_logs',
                  'rail_refresh_watermarks',
                  'reconciliation_findings',
                  'reconciliation_state',
                  'reprice_batches',
                  'solana_subscriptions',
                  'subscription_reprices',
                  'subscription_status_transitions',
                  'subscriptions',
                  'usage_events',
                  'webhook_events',
                  'webhook_health',
                  'webhook_health_daily',
                  'worker_state']::text[])
        LOOP
            EXECUTE format('SELECT EXISTS (SELECT 1 FROM %I.%I WHERE merchant_id=$1)',TG_TABLE_SCHEMA,item.relname)
                INTO occupied USING NEW.merchant_id;
            IF occupied THEN RAISE EXCEPTION 'billing restore destination is not empty: %',item.relname USING ERRCODE='55000'; END IF;
        END LOOP;
    ELSE
        IF OLD.kind<>'billing_restore' OR NEW.kind<>'billing_restore'
           OR OLD.status<>'running' OR NEW.status<>'completed'
           OR NEW.finished_at IS NULL
           OR (to_jsonb(NEW)-ARRAY['status','finished_at','summary','run_class']) IS DISTINCT FROM (to_jsonb(OLD)-ARRAY['status','finished_at','summary','run_class'])
           OR NOT EXISTS (SELECT 1 FROM openrails.maintenance_runs r WHERE r.id=OLD.id AND r.merchant_id=OLD.merchant_id
               AND r.xmin=pg_current_xact_id_if_assigned()::xid)
           OR OLD.id::text IS DISTINCT FROM current_setting('app.billing_restore_id',true)
           OR NEW.summary->>'digest' IS NULL OR NEW.summary->>'digest' !~ '^[0-9a-f]{64}$'
           OR jsonb_typeof(NEW.summary->'rows') IS DISTINCT FROM 'number'
           OR (NEW.summary->>'rows')::numeric < 0
           OR (NEW.summary->>'rows')::numeric <> trunc((NEW.summary->>'rows')::numeric) THEN
            RAISE EXCEPTION 'invalid billing restore receipts transition' USING ERRCODE='23514';
        END IF;
        PERFORM openrails.check_billing_restore_ledger(NEW.merchant_id);
    END IF;
    RETURN NEW;
END;
$$;
