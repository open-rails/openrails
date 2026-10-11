-- parent: 55 sha256:70665c5081ec44999b2fef3f32c7194273a2d822c145df58595e80dc41ae9193
-- Repair: none-needed A new table; links are made from now on.
-- An openrails.dev account linked to one merchant's customer, proven from both
-- sides in one exchange. customers gains nothing: the link points at the
-- customer, never the reverse.
CREATE TABLE billing.customer_links (
    merchant_id uuid NOT NULL,
    id uuid DEFAULT uuidv7() NOT NULL,
    customer_id uuid NOT NULL,
    account_id uuid NOT NULL,
    method text NOT NULL,
    credential text,
    order_id uuid,
    auth_time timestamp with time zone,
    amr text[],
    terms_version text NOT NULL,
    status text NOT NULL,
    linked_at timestamp with time zone DEFAULT now() NOT NULL,
    revoked_at timestamp with time zone,
    revoked_by text,
    CONSTRAINT customer_links_method_check CHECK (method IN ('checkout', 'connect', 'sign_in')),
    CONSTRAINT customer_links_status_check CHECK (status IN ('active', 'revoked')),
    CONSTRAINT customer_links_revoked_by_check CHECK (revoked_by IN ('customer', 'account_deleted', 'merchant_left')),
    CONSTRAINT customer_links_revocation_check CHECK (((status = 'revoked') = (revoked_at IS NOT NULL)) AND ((revoked_at IS NULL) = (revoked_by IS NULL)) AND (revoked_at IS NULL OR revoked_at >= linked_at)),
    CONSTRAINT customer_links_proof_check CHECK (
        (method = 'checkout' AND credential = 'checkout_url' AND order_id IS NOT NULL AND auth_time IS NOT NULL AND amr IS NOT NULL)
        OR (method = 'connect' AND credential = 'customer_token' AND order_id IS NULL AND auth_time IS NOT NULL AND amr IS NOT NULL)
        OR (method = 'sign_in' AND credential IS NULL AND order_id IS NULL AND auth_time IS NULL AND amr IS NULL AND customer_id = account_id)),
    CONSTRAINT customer_links_terms_version_check CHECK ((terms_version = btrim(terms_version)) AND (terms_version <> ''))
);
COMMENT ON TABLE billing.customer_links IS 'An openrails.dev account linked to one merchant''s customer: unique while active per customer and per (account, merchant). openrails.dev''s data, not the merchant''s: never archived, revoked when the merchant leaves. Retention: revoked links are deleted 24 months (731 days) after revoked_at; an active link is kept while it lasts.';
COMMENT ON COLUMN billing.customer_links.account_id IS 'The openrails.dev account: its AuthKit user id, the public network id (OIDC sub).';
COMMENT ON COLUMN billing.customer_links.method IS 'checkout: at hosted or embedded checkout; connect: the merchant''s own customer, from its site; sign_in: the merchant signs its customers in with openrails.dev, so the customer is the account itself.';
COMMENT ON COLUMN billing.customer_links.credential IS 'The merchant-side proof: the customer''s token at the merchant, or the checkout URL secret of order_id. NULL for sign_in.';
COMMENT ON COLUMN billing.customer_links.auth_time IS 'The network sign-in the link was proven with (OIDC auth_time, amr). NULL for sign_in.';
COMMENT ON COLUMN billing.customer_links.order_id IS 'The order whose checkout made the link; evidence only, kept after the order goes.';
COMMENT ON COLUMN billing.customer_links.terms_version IS 'The network-terms version the account had accepted when linked.';

ALTER TABLE ONLY billing.customer_links
    ADD CONSTRAINT customer_links_pkey PRIMARY KEY (merchant_id, id);
ALTER TABLE ONLY billing.customer_links
    ADD CONSTRAINT customer_links_customer_fkey FOREIGN KEY (merchant_id, customer_id) REFERENCES billing.customers(merchant_id, id) ON DELETE RESTRICT;
CREATE UNIQUE INDEX customer_links_customer_active_key ON billing.customer_links USING btree (merchant_id, customer_id) WHERE (status = 'active');
CREATE UNIQUE INDEX customer_links_account_active_key ON billing.customer_links USING btree (account_id, merchant_id) WHERE (status = 'active');
CREATE INDEX customer_links_revoked_at_idx ON billing.customer_links USING btree (merchant_id, revoked_at) WHERE (status = 'revoked');

-- Host events of links.
ALTER TABLE billing.host_outbox DROP CONSTRAINT host_outbox_currency_check;
ALTER TABLE billing.host_outbox ADD CONSTRAINT host_outbox_currency_check CHECK ((currency IS NULL) = (event_type IN ('product.entitlements_changed', 'customer.network_linked', 'customer.network_unlinked')) AND (currency IS NULL OR currency ~ '^[A-Z0-9]{3,12}$'));
ALTER TABLE billing.host_outbox DROP CONSTRAINT host_outbox_payload_check;
ALTER TABLE billing.host_outbox ADD CONSTRAINT host_outbox_payload_check CHECK (
    (event_type = 'payment.settled' AND subject_type = 'payment' AND payment_id IS NOT NULL
     AND subject_id = payment_id AND amount IS NOT NULL AND amount > 0 AND data = '{}'::jsonb)
    OR (event_type IN ('delinquency.grace', 'delinquency.entered', 'delinquency.cleared')
        AND subject_type = 'customer' AND payment_id IS NULL AND amount IS NULL)
    OR (event_type = 'product.entitlements_changed' AND subject_type = 'product' AND payment_id IS NULL AND amount IS NULL)
    OR (event_type IN ('order.completed', 'order.requires_action', 'order.payment_failed', 'order.canceled', 'order.expired')
        AND subject_type = 'order' AND payment_id IS NULL AND amount IS NOT NULL AND amount >= 0 AND currency IS NOT NULL)
    OR (event_type IN ('customer.network_linked', 'customer.network_unlinked')
        AND subject_type = 'customer' AND payment_id IS NULL AND amount IS NULL)
);
COMMENT ON TABLE billing.host_outbox IS 'Typed durable host events: successful rail payment settlements, delinquency lifecycle transitions, product key changes, order transitions and customer links. Acknowledge after idempotent processing; acknowledgments are separate from notification read state. Retention: delivered events are deleted 30 days after delivered_at; an undelivered event is never deleted.';

-- Links are merchant rows and occupy a restore destination.
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
                  'carts',
                  'catalog_applications',
                  'catalog_field_owners',
                  'catalog_meters',
                  'catalog_rate_cards',
                  'catalog_restore_receipts',
                  'checkout_attempts',
                  'checkout_sessions',
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
                  'invoice_collection_cadence',
                  'invoice_items',
                  'invoices',
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
                  'scheduled_changes',
                  'solana_pay_receipts',
                  'solana_pay_references',
                  'solana_subscriptions',
                  'subscription_status_transitions',
                  'subscription_verifications',
                  'subscriptions',
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
