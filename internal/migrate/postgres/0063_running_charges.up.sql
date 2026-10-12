-- Repair: none-needed New tables hold no rows, and the widened host_outbox check refuses no stored row.
-- A running charge: a customer charged at a rate per hour from started_at
-- until it is stopped. Nothing is written while it runs. The charge so far is
-- running_charge_total; what the ledger has not been posted is held against
-- the balance by the held-total queries.

CREATE FUNCTION billing.running_charge_total(p_rate_per_hour bigint, p_started_at timestamptz, p_at timestamptz) RETURNS bigint
    LANGUAGE sql IMMUTABLE PARALLEL SAFE
    AS $$
SELECT div(p_rate_per_hour::numeric * greatest(floor(extract(epoch FROM p_at - p_started_at)), 0) * 2 + 3600, 7200)::bigint
$$;
COMMENT ON FUNCTION billing.running_charge_total(p_rate_per_hour bigint, p_started_at timestamptz, p_at timestamptz) IS 'What a running charge has charged by p_at: its rate times the whole seconds since p_started_at (never negative), over 3600, rounded half up.';

CREATE TABLE billing.running_charges (
    id uuid DEFAULT uuidv7() NOT NULL,
    merchant_id uuid NOT NULL,
    reference text NOT NULL,
    customer_id uuid NOT NULL,
    currency text NOT NULL,
    rate_per_hour bigint NOT NULL,
    overdraft_amount bigint NOT NULL,
    low_balance_amount bigint NOT NULL,
    resource text,
    started_at timestamp with time zone NOT NULL,
    stopped_at timestamp with time zone,
    posted_through_at timestamp with time zone NOT NULL,
    posted_amount bigint NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT running_charges_pkey PRIMARY KEY (merchant_id, id),
    CONSTRAINT running_charges_reference_key UNIQUE (merchant_id, reference),
    CONSTRAINT running_charges_merchant_id_fkey FOREIGN KEY (merchant_id) REFERENCES billing.merchants(id) ON DELETE RESTRICT,
    CONSTRAINT running_charges_customer_id_fkey FOREIGN KEY (merchant_id, customer_id) REFERENCES billing.customers(merchant_id, id) ON DELETE RESTRICT,
    CONSTRAINT running_charges_reference_check CHECK (reference <> '' AND reference = btrim(reference) AND octet_length(reference) <= 255),
    CONSTRAINT running_charges_currency_check CHECK (currency ~ '^[A-Z0-9]{3,12}$'),
    CONSTRAINT running_charges_rate_per_hour_check CHECK (rate_per_hour > 0 AND rate_per_hour <= 1000000000000),
    CONSTRAINT running_charges_overdraft_amount_check CHECK (overdraft_amount >= 0),
    CONSTRAINT running_charges_low_balance_amount_check CHECK (low_balance_amount >= 0),
    CONSTRAINT running_charges_resource_check CHECK (resource <> '' AND octet_length(resource) <= 255),
    CONSTRAINT running_charges_posted_check CHECK (posted_through_at >= started_at AND posted_amount = billing.running_charge_total(rate_per_hour, started_at, posted_through_at)),
    CONSTRAINT running_charges_stopped_at_check CHECK (stopped_at IS NULL OR stopped_at = posted_through_at)
);
COMMENT ON TABLE billing.running_charges IS 'One row per running charge: a customer charged rate_per_hour (native units of currency) from started_at until stopped_at. No row changes while it runs; a posting moves what accrued through posted_through_at to the ledger, and a stopped charge is posted through stopped_at. Retention: permanent, never pruned.';
COMMENT ON COLUMN billing.running_charges.reference IS 'The host''s name for what it charges for: unique per merchant, and the identity a repeated start or stop is matched on.';
COMMENT ON COLUMN billing.running_charges.overdraft_amount IS 'How far below zero this charge lets a prepaid customer''s capacity reach; the smallest among a customer''s running charges applies.';
COMMENT ON COLUMN billing.running_charges.low_balance_amount IS 'The balance under which the customer is told it is low; the largest among a customer''s running charges applies, 0 is no line.';
COMMENT ON COLUMN billing.running_charges.posted_amount IS 'What the ledger has been posted: running_charge_total through posted_through_at.';

CREATE INDEX running_charges_customer_id_currency_idx ON billing.running_charges USING btree (merchant_id, customer_id, currency) WHERE (stopped_at IS NULL);

CREATE TRIGGER immutable_running_charge_facts BEFORE UPDATE OR DELETE ON billing.running_charges
FOR EACH ROW EXECUTE FUNCTION billing.guard_billing_fact_columns('stopped_at','posted_through_at','posted_amount','updated_at');

CREATE TABLE billing.balance_watches (
    merchant_id uuid NOT NULL,
    customer_id uuid NOT NULL,
    currency text NOT NULL,
    next_check_at timestamp with time zone NOT NULL,
    depletes_at timestamp with time zone,
    low_notified_at timestamp with time zone,
    depleted_at timestamp with time zone,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT balance_watches_pkey PRIMARY KEY (merchant_id, customer_id, currency),
    CONSTRAINT balance_watches_merchant_id_fkey FOREIGN KEY (merchant_id) REFERENCES billing.merchants(id) ON DELETE RESTRICT,
    CONSTRAINT balance_watches_customer_id_fkey FOREIGN KEY (merchant_id, customer_id) REFERENCES billing.customers(merchant_id, id) ON DELETE RESTRICT,
    CONSTRAINT balance_watches_currency_check CHECK (currency ~ '^[A-Z0-9]{3,12}$')
);
COMMENT ON TABLE billing.balance_watches IS 'One row per customer and currency with a running charge: when its balance is next looked at (the low line, the run-out or the next 00:00 UTC, whichever is first) and what the last look found. Derived from the running charges and the balance; stopping the last charge deletes the row.';
COMMENT ON COLUMN billing.balance_watches.depletes_at IS 'When the customer''s capacity reaches zero at the rate of its running charges, as last computed; in the past while it is at zero.';
COMMENT ON COLUMN billing.balance_watches.low_notified_at IS 'When balance.low was sent for the current crossing; null once the balance is back above the line.';
COMMENT ON COLUMN billing.balance_watches.depleted_at IS 'When balance.depleted was sent for the current crossing; null once capacity is back above zero.';

CREATE INDEX balance_watches_next_check_at_idx ON billing.balance_watches USING btree (next_check_at);
CREATE INDEX balance_watches_depletes_at_idx ON billing.balance_watches USING btree (merchant_id, depletes_at);

-- The two balance events are host events about a customer in one currency.
ALTER TABLE billing.host_outbox DROP CONSTRAINT host_outbox_payload_check;
ALTER TABLE billing.host_outbox ADD CONSTRAINT host_outbox_payload_check CHECK (
    (event_type = 'payment.settled' AND subject_type = 'payment' AND payment_id IS NOT NULL
     AND subject_id = payment_id AND amount IS NOT NULL AND amount > 0 AND data = '{}'::jsonb)
    OR (event_type IN ('delinquency.grace', 'delinquency.entered', 'delinquency.cleared', 'balance.low', 'balance.depleted')
        AND subject_type = 'customer' AND payment_id IS NULL AND amount IS NULL)
    OR (event_type = 'product.entitlements_changed' AND subject_type = 'product' AND payment_id IS NULL AND amount IS NULL)
    OR (event_type IN ('order.completed', 'order.requires_action', 'order.payment_failed', 'order.canceled', 'order.expired', 'order.voided')
        AND subject_type = 'order' AND payment_id IS NULL AND amount IS NOT NULL AND amount >= 0 AND currency IS NOT NULL)
    OR (event_type IN ('customer.network_linked', 'customer.network_unlinked', 'customer.payment_authority_revoked')
        AND subject_type = 'customer' AND payment_id IS NULL AND amount IS NULL)
);
COMMENT ON TABLE billing.host_outbox IS 'Typed durable host events: successful rail payment settlements, delinquency lifecycle transitions, product key changes, order transitions, customer links, payment authority revocations and the low and depleted balance of a customer with a running charge. Acknowledge after idempotent processing; acknowledgments are separate from notification read state. Retention: delivered events are deleted 30 days after delivered_at; an undelivered event is never deleted.';

-- Running charges and their watches are merchant rows and occupy a restore destination.
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
                  'metered_rating_watermarks',
                  'money_settings',
                  'nmi_bulk_checkpoints',
                  'nmi_history_months',
                  'nmi_history_reads',
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
                  'product_archive_operations',
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
