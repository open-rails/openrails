-- parent: 39 sha256:4febe204bff455254954b55647a62949020c39c17d157e98a59d40fbbc350541
-- A payment method is the card account the customer chose; its issuer and
-- the networks maintain it. Every change to the card is an append-only
-- version, and the method has a lifecycle: active, then closed by its bank,
-- replaced by another method, or removed.
CREATE TABLE billing.payment_method_versions (
    id uuid DEFAULT uuidv7() NOT NULL,
    merchant_id uuid NOT NULL,
    customer_id uuid NOT NULL,
    payment_method_id uuid NOT NULL,
    source text NOT NULL,
    kind text NOT NULL,
    event_ref text NOT NULL,
    psp_id uuid,
    custodian_id uuid,
    rail_customer_ref text,
    rail_method_ref text,
    card_brand text,
    card_last4 text,
    card_exp_month smallint,
    card_exp_year smallint,
    fingerprint text,
    network_token_id text,
    network_token_status text,
    network_token_par text,
    effective_at timestamp with time zone NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT payment_method_versions_pkey PRIMARY KEY (merchant_id, id),
    CONSTRAINT payment_method_versions_source_check CHECK (source IN ('customer_save', 'customer_edit', 'stripe_updater', 'nmi_acu', 'basis_theory_updater', 'hyperswitch_updater', 'network_token', 'provider_read')),
    CONSTRAINT payment_method_versions_kind_check CHECK (kind IN ('saved', 'updated', 'brand_changed', 'closed', 'contact_cardholder')),
    CONSTRAINT payment_method_versions_event_ref_check CHECK (event_ref <> ''),
    CONSTRAINT payment_method_versions_holder_check CHECK ((psp_id IS NULL) <> (custodian_id IS NULL)),
    CONSTRAINT payment_method_versions_rail_customer_ref_check CHECK (rail_customer_ref <> ''),
    CONSTRAINT payment_method_versions_rail_method_ref_check CHECK (rail_method_ref <> ''),
    CONSTRAINT payment_method_versions_card_brand_check CHECK (card_brand <> ''),
    CONSTRAINT payment_method_versions_card_last4_check CHECK (card_last4 ~ '^[0-9]{4}$'),
    CONSTRAINT payment_method_versions_card_exp_month_check CHECK (card_exp_month >= 1 AND card_exp_month <= 12),
    CONSTRAINT payment_method_versions_card_exp_year_check CHECK (card_exp_year >= 2000 AND card_exp_year <= 2199),
    CONSTRAINT payment_method_versions_card_expiry_pair_check CHECK ((card_exp_month IS NULL) = (card_exp_year IS NULL)),
    CONSTRAINT payment_method_versions_fingerprint_check CHECK (fingerprint <> ''),
    CONSTRAINT payment_method_versions_network_token_id_check CHECK (network_token_id <> ''),
    CONSTRAINT payment_method_versions_network_token_status_check CHECK (network_token_status IN ('active', 'inactive', 'suspended', 'deleted')),
    CONSTRAINT payment_method_versions_network_token_par_check CHECK (network_token_par <> ''),
    CONSTRAINT payment_method_versions_event_key UNIQUE (merchant_id, payment_method_id, source, event_ref),
    CONSTRAINT payment_method_versions_payment_method_id_fkey FOREIGN KEY (merchant_id, customer_id, payment_method_id) REFERENCES billing.payment_methods(merchant_id, customer_id, id) ON DELETE CASCADE,
    CONSTRAINT payment_method_versions_psp_id_fkey FOREIGN KEY (merchant_id, psp_id) REFERENCES billing.psps(merchant_id, id) ON DELETE RESTRICT,
    CONSTRAINT payment_method_versions_custodian_id_fkey FOREIGN KEY (merchant_id, custodian_id) REFERENCES billing.custodians(merchant_id, id) ON DELETE RESTRICT
);
COMMENT ON TABLE billing.payment_method_versions IS 'Append-only history of the card behind a payment method: one row per save, customer edit, updater or network change, closure or contact advice, with the card as its holder reported it at that version. Replays of one source event are one row. Retention: permanent, never pruned.';
COMMENT ON COLUMN billing.payment_method_versions.source IS 'Who reported the change: customer_save, customer_edit, stripe_updater, nmi_acu, basis_theory_updater, hyperswitch_updater, network_token or provider_read (OpenRails read the holder).';
COMMENT ON COLUMN billing.payment_method_versions.kind IS 'saved; updated (a new number, expiry, holder handle or network token, under the same brand); brand_changed; closed; contact_cardholder.';
COMMENT ON COLUMN billing.payment_method_versions.event_ref IS 'The source''s event, notice or operation; one row per method, source and event.';
COMMENT ON COLUMN billing.payment_method_versions.psp_id IS 'The PSP holding the card at this version, for a PSP-held card; with custodian_id, the scope of its fingerprint.';
COMMENT ON COLUMN billing.payment_method_versions.custodian_id IS 'The custodian holding the card at this version, for a custodian-held card.';
COMMENT ON COLUMN billing.payment_method_versions.fingerprint IS 'The holder''s fingerprint of the card number at this version; a reissued number has a new one.';
COMMENT ON COLUMN billing.payment_method_versions.network_token_par IS 'Payment account reference, where the holder reports one.';
COMMENT ON COLUMN billing.payment_method_versions.effective_at IS 'When the change was learned.';

CREATE INDEX payment_method_versions_payment_method_id_effective_at_idx ON billing.payment_method_versions (merchant_id, payment_method_id, effective_at, id);
CREATE INDEX payment_method_versions_customer_id_fingerprint_idx ON billing.payment_method_versions (merchant_id, customer_id, fingerprint) WHERE fingerprint IS NOT NULL;
CREATE INDEX payment_method_versions_psp_id_idx ON billing.payment_method_versions (merchant_id, psp_id) WHERE psp_id IS NOT NULL;
CREATE INDEX payment_method_versions_custodian_id_idx ON billing.payment_method_versions (merchant_id, custodian_id) WHERE custodian_id IS NOT NULL;

CREATE FUNCTION billing.guard_payment_method_version_update() RETURNS trigger
    LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
BEGIN
    RAISE EXCEPTION 'card version % is append-only', OLD.id USING ERRCODE = '23514';
END;
$$;
CREATE TRIGGER guard_payment_method_version_update BEFORE UPDATE ON billing.payment_method_versions FOR EACH ROW EXECUTE FUNCTION billing.guard_payment_method_version_update();

-- The method's lifecycle. A closure, a replacement and a removal are final.
ALTER TABLE billing.payment_methods ADD COLUMN status text, ADD COLUMN replaced_by_id uuid, ADD COLUMN contact_cardholder_at timestamp with time zone;
UPDATE billing.payment_methods SET
    status = CASE
        WHEN park_reason IN ('nmi_acu_closed_account', 'bt_au_closed_account') THEN 'closed'
        -- Mastercard's CONTACT answer means the account is closed.
        WHEN park_reason = 'bt_au_contact_cardholder' AND regexp_replace(lower(coalesce(card_brand, '')), '[^a-z]', '', 'g') IN ('mastercard', 'mc') THEN 'closed'
        WHEN park_reason = 'stripe_payment_method_detached' THEN 'removed'
        ELSE 'active' END,
    contact_cardholder_at = CASE WHEN park_reason = 'bt_au_contact_cardholder'
        AND regexp_replace(lower(coalesce(card_brand, '')), '[^a-z]', '', 'g') NOT IN ('mastercard', 'mc') THEN parked_at END,
    park_reason = CASE WHEN park_reason IN ('nmi_acu_closed_account', 'bt_au_closed_account', 'bt_au_contact_cardholder', 'stripe_payment_method_detached') THEN NULL ELSE park_reason END,
    parked_at = CASE WHEN park_reason IN ('nmi_acu_closed_account', 'bt_au_closed_account', 'bt_au_contact_cardholder', 'stripe_payment_method_detached') THEN NULL ELSE parked_at END;
ALTER TABLE billing.payment_methods ALTER COLUMN status SET NOT NULL;
ALTER TABLE ONLY billing.payment_methods
    ADD CONSTRAINT payment_methods_status_check CHECK (status IN ('active', 'closed', 'replaced', 'removed')),
    ADD CONSTRAINT payment_methods_replaced_by_check CHECK ((status = 'replaced') = (replaced_by_id IS NOT NULL) AND replaced_by_id <> id),
    ADD CONSTRAINT payment_methods_replaced_by_id_fkey FOREIGN KEY (merchant_id, customer_id, replaced_by_id) REFERENCES billing.payment_methods(merchant_id, customer_id, id);
CREATE INDEX payment_methods_replaced_by_id_idx ON billing.payment_methods (merchant_id, replaced_by_id) WHERE replaced_by_id IS NOT NULL;
COMMENT ON TABLE billing.payment_methods IS 'The card account a customer chose, held at one PSP or custodian. Its card is the latest of its payment_method_versions; its issuer may reissue it under the same method.';
COMMENT ON COLUMN billing.payment_methods.status IS 'active; closed (the bank closed the account); replaced (another method took its place, replaced_by_id); removed. The last three are final.';
COMMENT ON COLUMN billing.payment_methods.replaced_by_id IS 'The method that replaced this one, when status is replaced.';
COMMENT ON COLUMN billing.payment_methods.contact_cardholder_at IS 'When the issuer last asked for the cardholder to be contacted; cleared by the next change to the card.';
COMMENT ON COLUMN billing.payment_methods.fingerprint IS 'The holder''s fingerprint of the card''s current number; earlier numbers'' are in payment_method_versions.';
COMMENT ON COLUMN billing.payment_methods.park_reason IS 'Non-empty while the method cannot be charged for a holder-side reason (a deletion in flight, a custody token gone): charges fail loudly and nothing is canceled because of it.';

CREATE FUNCTION billing.guard_payment_method_status() RETURNS trigger
    LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
BEGIN
    IF OLD.status <> 'active' AND (NEW.status, NEW.replaced_by_id) IS DISTINCT FROM (OLD.status, OLD.replaced_by_id) THEN
        RAISE EXCEPTION 'payment method % is %, which is final', OLD.id, OLD.status USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER guard_payment_method_status BEFORE UPDATE OF status, replaced_by_id ON billing.payment_methods FOR EACH ROW EXECUTE FUNCTION billing.guard_payment_method_status();

-- A closed or removed card's agreements end with it.
UPDATE billing.mandates m SET status = 'ended',
    end_reason = CASE pm.status WHEN 'closed' THEN 'closed' ELSE 'payment_method_removed' END,
    ended_at = now(), updated_at = now()
FROM billing.payment_methods pm
WHERE pm.merchant_id = m.merchant_id AND pm.id = m.payment_method_id
  AND pm.status IN ('closed', 'removed') AND m.status IN ('active', 'requires_reconsent');

-- Each method's history starts with its card as it is now. The updates table
-- recorded only the kind of each change, never the card, so it is not carried.
INSERT INTO billing.payment_method_versions (merchant_id, customer_id, payment_method_id, source, kind, event_ref, psp_id, custodian_id,
    rail_customer_ref, rail_method_ref, card_brand, card_last4, card_exp_month, card_exp_year, fingerprint,
    network_token_id, network_token_status, network_token_par, effective_at)
SELECT merchant_id, customer_id, id, 'customer_save', 'saved', 'created', psp_id, custodian_id,
    rail_customer_ref, rail_method_ref, NULLIF(card_brand, ''), card_last4, card_exp_month, card_exp_year, fingerprint,
    network_token_id, network_token_status, network_token_par, created_at
FROM billing.payment_methods;
DROP TABLE billing.payment_method_updates;

-- Card versions are merchant rows and occupy a restore destination.
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
                  'billing_policies',
                  'billing_policy_bindings',
                  'card_attempt_failures',
                  'catalog_applications',
                  'catalog_meters',
                  'catalog_rate_cards',
                  'catalog_restore_receipts',
                  'checkout_attempts',
                  'checkout_sessions',
                  'cost_observations',
                  'cost_qualifications',
                  'cost_refusals',
                  'cost_resolutions',
                  'credential_publications',
                  'custodians',
                  'custody_migrations',
                  'customer_delinquency',
                  'customer_entitlement_cache',
                  'customer_entitlement_cache_stamps',
                  'customer_invoice_profiles',
                  'customers',
                  'dashboard_configs',
                  'document_sequences',
                  'failed_usage_windows',
                  'destructive_run_before_images',
                  'grants',
                  'host_outbox',
                  'idempotency_keys',
                  'invoice_collection_cadence',
                  'invoice_items',
                  'invoices',
                  'invoker_spend_limits',
                  'ledger_accounts',
                  'ledger_transfers',
                  'maintenance_runs',
                  'mandates',
                  'merchant_configuration_applications',
                  'merchant_configurations',
                  'merchant_deks',
                  'merchant_destructive_policy',
                  'merchant_secrets',
                  'merchant_webhooks',
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
                  'price_migrations',
                  'price_psp_bindings',
                  'prices',
                  'product_archive_operations',
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
