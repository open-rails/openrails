-- parent: 24 sha256:991d4ede1222d8df27c0b57cb056adf11b31f57725e2873f4d3beb08491df8b1
-- Repair: none-needed cost_resolutions' new key names cost_refusals, which this file fills for every refused qualification before the key, and every existing resolution closes one.
-- A hold whose provider cost will not qualify automatically waits for an
-- operator. Until now only a refused qualification said so: a host that cannot
-- produce evidence at all (no provable lifecycle, a provider that reports no
-- billing, evidence OpenRails rejects) had no way to record it, and the hold's
-- only exit was a release asserting the provider operation never happened.
-- cost_refusals is the one record of every stuck hold: the qualifier writes it
-- when it refuses a qualification, the host when it cannot qualify. It fences
-- observation, extension and release; an operator's resolution is its only exit.
CREATE TABLE billing.cost_refusals (
    merchant_id uuid NOT NULL,
    operation_id text NOT NULL,
    reason text NOT NULL,
    qualification_state text GENERATED ALWAYS AS (
        CASE WHEN reason = ANY (ARRAY['provider_evidence_refused'::text, 'negative_or_corrective_record'::text, 'decreasing_provider_cost'::text])
             THEN 'refused'::text END) STORED,
    detail text,
    refused_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT cost_refusals_reason_check CHECK ((reason = ANY (ARRAY['provider_evidence_refused'::text, 'negative_or_corrective_record'::text, 'decreasing_provider_cost'::text, 'lifecycle_unprovable'::text, 'provider_billing_unavailable'::text, 'observation_rejected'::text]))),
    CONSTRAINT cost_refusals_detail_check CHECK (((detail IS NULL) OR ((detail <> ''::text) AND (detail = btrim(detail)) AND (octet_length(detail) <= 4096))))
);
COMMENT ON TABLE billing.cost_refusals IS 'An operation authorization whose provider cost will not qualify automatically: refused by the qualifier (its qualification is refused) or by the host (it cannot produce evidence). The hold waits for an operator''s resolution, its only exit. Immutable. Retention: permanent, never pruned.';

ALTER TABLE ONLY billing.cost_refusals
    ADD CONSTRAINT cost_refusals_pkey PRIMARY KEY (merchant_id, operation_id);
ALTER TABLE ONLY billing.cost_refusals
    ADD CONSTRAINT cost_refusals_authorization_fkey FOREIGN KEY (merchant_id, operation_id) REFERENCES billing.operation_authorizations(merchant_id, operation_id) ON DELETE RESTRICT;
-- The qualifier's refusal stands on its refused qualification and keeps it refused.
ALTER TABLE ONLY billing.cost_refusals
    ADD CONSTRAINT cost_refusals_qualification_fkey FOREIGN KEY (merchant_id, operation_id, qualification_state) REFERENCES billing.cost_qualifications(merchant_id, operation_id, state) ON UPDATE RESTRICT ON DELETE RESTRICT;

CREATE TRIGGER immutable_cost_refusals BEFORE UPDATE OR DELETE ON billing.cost_refusals
FOR EACH ROW EXECUTE FUNCTION billing.reject_immutable_billing_fact();

-- Every refused qualification already waits for an operator.
INSERT INTO billing.cost_refusals (merchant_id, operation_id, reason, detail, refused_at)
SELECT q.merchant_id, q.operation_id, q.reason,
       (SELECT 'observation ' || o.observation_id FROM billing.cost_observations o
         WHERE o.merchant_id = q.merchant_id AND o.operation_id = q.operation_id
         ORDER BY o.observed_at DESC LIMIT 1),
       q.updated_at
FROM billing.cost_qualifications q
WHERE q.state = 'refused';

-- A resolution closes a refused hold, which may have no qualification at all,
-- so it now stands on the hold's refusal instead of a refused qualification.
-- Looser, not narrower: every refused qualification has a refusal (above), so
-- every existing resolution satisfies the new key.
ALTER TABLE billing.cost_resolutions DROP CONSTRAINT cost_resolutions_qualification_fkey;
ALTER TABLE ONLY billing.cost_resolutions
    ADD CONSTRAINT cost_resolutions_refusal_fkey FOREIGN KEY (merchant_id, operation_id) REFERENCES billing.cost_refusals(merchant_id, operation_id) ON DELETE RESTRICT;
COMMENT ON TABLE billing.cost_resolutions IS 'An operator''s attested close of a refused operation authorization (see cost_refusals): settled at the attested provider cost (pass-through, so the customer is charged it) or written off (released, the customer is not charged). Immutable. Retention: permanent, never pruned.';
COMMENT ON COLUMN billing.cost_resolutions.qualification_state IS 'Always refused: a resolution closes a refused hold.';

-- The operator's list of holds, newest first, by state.
CREATE INDEX operation_authorizations_state_created_at_idx ON billing.operation_authorizations USING btree (merchant_id, state, created_at DESC, operation_id DESC);

-- Refusals are merchant rows and occupy a restore destination.
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
                  'destructive_run_before_images',
                  'grants',
                  'host_outbox',
                  'idempotency_keys',
                  'invoice_collection_cadence',
                  'invoice_items',
                  'invoice_payments',
                  'invoices',
                  'invoker_spend_limits',
                  'ledger_accounts',
                  'ledger_transfers',
                  'maintenance_runs',
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
                  'payment_attempts',
                  'payment_method_updates',
                  'payment_methods',
                  'payments',
                  'price_key_movements',
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
                  'reprice_batches',
                  'solana_pay_receipts',
                  'solana_pay_references',
                  'solana_subscriptions',
                  'subscription_reprices',
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
