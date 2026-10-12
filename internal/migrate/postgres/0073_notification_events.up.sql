-- parent: 72 sha256:de38170fa42bc1f56d0f59653635d57077ab1c71b267dcc348d554e6eaafb854
-- Repair: none-needed Legacy customer rows are copied before being removed.
-- Applications own inbox/read/channel policy. These immutable facts survive
-- independent consumer acknowledgment and the legacy inbox's supersession.
CREATE TABLE billing.notification_events (
    merchant_id uuid NOT NULL REFERENCES billing.merchants(id),
    id uuid NOT NULL,
    customer_id uuid NOT NULL,
    event_type text NOT NULL,
    data jsonb NOT NULL DEFAULT '{}',
    created_at timestamptz NOT NULL DEFAULT now(),
    inserted_at timestamptz NOT NULL DEFAULT now(),
    settled_at timestamptz,
    PRIMARY KEY (merchant_id,id),
    FOREIGN KEY (merchant_id,customer_id) REFERENCES billing.customers(merchant_id,id) ON DELETE CASCADE
);
CREATE INDEX notification_events_delivery_idx ON billing.notification_events(inserted_at,merchant_id,id);
CREATE TABLE billing.notification_consumers (
    id uuid PRIMARY KEY DEFAULT uuidv7(),
    merchant_id uuid REFERENCES billing.merchants(id),
    owner text NOT NULL CHECK (length(owner) BETWEEN 1 AND 512),
    name text NOT NULL CHECK (name ~ '^[A-Za-z0-9_.-]{1,64}$'),
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE NULLS NOT DISTINCT (merchant_id,owner,name)
);
CREATE TABLE billing.notification_event_acks (
    consumer_id uuid NOT NULL REFERENCES billing.notification_consumers(id) ON DELETE CASCADE,
    merchant_id uuid NOT NULL,
    event_id uuid NOT NULL,
    acknowledged_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY(consumer_id,merchant_id,event_id),
    FOREIGN KEY(merchant_id,event_id) REFERENCES billing.notification_events(merchant_id,id) ON DELETE CASCADE
);
INSERT INTO billing.notification_events(merchant_id,id,customer_id,event_type,data,created_at)
SELECT merchant_id,id,customer_id,event_type,COALESCE(data,'{}')-'user_email',created_at
FROM billing.notifications WHERE recipient_kind='customer';
DELETE FROM billing.notifications WHERE recipient_kind='customer';
-- The remaining legacy merchant-alert table is not the customer source.
ALTER TABLE billing.notifications ADD CONSTRAINT notifications_operator_only CHECK (recipient_kind='merchant');
CREATE FUNCTION billing.guard_notification_event() RETURNS trigger
LANGUAGE plpgsql SET search_path TO 'pg_catalog','billing','pg_temp' AS $$
BEGIN
    IF (to_jsonb(OLD)-'settled_at') IS DISTINCT FROM (to_jsonb(NEW)-'settled_at') THEN
        RAISE EXCEPTION 'notification events are immutable' USING ERRCODE='23514';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER notification_event_immutable BEFORE UPDATE ON billing.notification_events
FOR EACH ROW EXECUTE FUNCTION billing.guard_notification_event();

-- Delivery records are not restored over an occupied destination.
CREATE OR REPLACE FUNCTION billing.guard_billing_restore_receipt() RETURNS trigger
    LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
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
                  'access_cutover_approvals',
                  'account_updater_batches',
                  'admission_denials_hourly',
                  'admission_operations',
                  'balance_watches',
                  'carts',
                  'catalog_applications',
                  'catalog_field_owners',
                  'catalog_meters',
                  'catalog_rate_cards',
                  'catalog_restore_receipts',
                  'checkout_attempts',
                  'cost_observations',
                  'cost_qualifications',
                  'cost_refusals',
                  'cost_resolutions',
                  'custodians',
                  'custody_migrations',
                  'customer_delinquency',
                  'customer_entitlement_cache',
                  'customer_entitlement_cache_stamps',
                  'customer_invoice_profiles',
                  'customer_links',
                  'customers',
                  'dashboard_configs',
                  'destructive_run_before_images',
                  'document_sequences',
                  'failed_usage_windows',
                  'grants',
                  'host_outbox',
                  'idempotency_keys',
                  'ledger_accounts',
                  'ledger_transfers',
                  'maintenance_runs',
                  'mandates',
                  'merchant_destructive_policy',
                  'money_settings',
                  'nmi_bulk_checkpoints',
                  'nmi_history_months',
                  'nmi_history_reads',
                  'notification_event_acks',
                  'notification_events',
                  'notifications',
                  'operation_authorization_extensions',
                  'operation_authorizations',
                  'order_lines',
                  'orders',
                  'owed_items',
                  'ownership_claims',
                  'payment_attempts',
                  'payment_method_versions',
                  'payment_methods',
                  'payments',
                  'price_key_movements',
                  'price_keys',
                  'price_migrations',
                  'price_psp_bindings',
                  'prices',
                  'product_access',
                  'product_entitlements',
                  'products',
                  'provider_intents',
                  'provider_mutation_logs',
                  'psp_customers',
                  'psp_refresh_watermarks',
                  'psps',
                  'rebill_cycles',
                  'reconciliation_findings',
                  'reconciliation_state',
                  'running_charges',
                  'scheduled_changes',
                  'solana_pay_receipts',
                  'solana_pay_references',
                  'solana_subscriptions',
                  'subscription_status_transitions',
                  'subscription_verifications',
                  'subscriptions',
                  'usage_collection_cadence',
                  'usage_events',
                  'webhook_events',
                  'webhook_health',
                  'webhook_health_daily']::text[])
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
           OR NOT EXISTS (SELECT 1 FROM billing.maintenance_runs r WHERE r.id=OLD.id AND r.merchant_id=OLD.merchant_id
               AND r.xmin=pg_current_xact_id_if_assigned()::xid)
           OR OLD.id::text IS DISTINCT FROM current_setting('openrails.billing_restore_id',true)
           OR NEW.summary->>'digest' IS NULL OR NEW.summary->>'digest' !~ '^[0-9a-f]{64}$'
           OR jsonb_typeof(NEW.summary->'rows') IS DISTINCT FROM 'number'
           OR (NEW.summary->>'rows')::numeric < 0
           OR (NEW.summary->>'rows')::numeric <> trunc((NEW.summary->>'rows')::numeric) THEN
            RAISE EXCEPTION 'invalid billing restore receipts transition' USING ERRCODE='23514';
        END IF;
        PERFORM billing.check_billing_restore_ledger(NEW.merchant_id);
    END IF;
    RETURN NEW;
END;
$$;

COMMENT ON TABLE billing.notification_events IS 'Immutable customer billing facts, independent of application inbox state. Retention: events are deleted 30 days after every active scoped and global consumer acknowledges; pending events never expire.';
