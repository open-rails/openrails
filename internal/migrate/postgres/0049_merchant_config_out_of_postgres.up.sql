-- parent: 48 sha256:523ded1436577062f7cdccb4fcdedabf7fbf328d2d84ae6cb51b92708e644170
-- Repair: none-needed Merchant configuration lives in a file or in Vault: export it before upgrading (dump-merchant-config); Postgres keeps identities and what OpenRails discovers.
-- A merchant's configuration (display name, settings, billing policies, alert
-- webhooks, PSPs and custodians with their settings, credentials and archived
-- state) lives in a file or in Vault, never here. Postgres keeps the
-- identities history points at and the state OpenRails itself discovers.

-- A customer's own billing policy is staff-set customer data: it moves beside
-- the customer before the bindings go.
ALTER TABLE billing.customers ADD COLUMN billing_policy text CHECK (billing_policy <> '');
COMMENT ON COLUMN billing.customers.billing_policy IS 'The billing policy staff assigned this customer, by its name in the merchant''s settings; NULL follows the tier, then the default.';
UPDATE billing.customers c SET billing_policy = b.policy_name
FROM billing.billing_policy_bindings b
WHERE b.merchant_id = c.merchant_id AND b.customer_id = c.id;

DROP TABLE billing.billing_policy_bindings;
DROP TABLE billing.billing_policies;
DROP TABLE billing.merchant_configuration_applications;
DROP TABLE billing.merchant_configurations;
DROP TABLE billing.merchant_secrets;
DROP TABLE billing.merchant_deks;
DROP TABLE billing.merchant_webhooks;
DROP TABLE billing.credential_publications;
DROP FUNCTION billing.lock_merchant_configuration_write();

ALTER TABLE billing.merchants DROP COLUMN display_name;

-- A PSP key names one current PSP. An identity its key no longer names (a
-- rotated Solana signer, an account the document changed) is superseded and
-- drains. Rows sharing a key keep the live, then the newest, one current.
ALTER TABLE billing.psps ADD COLUMN superseded_at timestamp with time zone;
COMMENT ON COLUMN billing.psps.superseded_at IS 'When OpenRails found the PSP''s key naming another account. A superseded PSP takes no new work and drains, as an archived one does.';
UPDATE billing.psps p SET superseded_at = COALESCE(p.archived_at, now())
WHERE EXISTS (
    SELECT 1 FROM billing.psps q
    WHERE q.merchant_id = p.merchant_id AND lower(q.key) = lower(p.key) AND q.id <> p.id
      AND ((p.archived AND NOT q.archived) OR (p.archived = q.archived AND (q.created_at, q.id) > (p.created_at, p.id)))
);
-- An archived PSP releases its gateway account's fingerprint.
UPDATE billing.psps SET credential_fingerprint = NULL WHERE archived OR superseded_at IS NOT NULL;

ALTER TABLE billing.psps
    DROP COLUMN settings,
    DROP COLUMN signer,
    DROP COLUMN credential_custody,
    DROP COLUMN credential_refs,
    DROP COLUMN credential_versions,
    DROP COLUMN retired_credentials,
    DROP COLUMN revision,
    DROP COLUMN archived,
    DROP COLUMN archived_at,
    DROP COLUMN webhook_overlap_expires_at,
    DROP COLUMN custodian_id;
CREATE INDEX customers_billing_policy_idx ON billing.customers USING btree (merchant_id, billing_policy) WHERE (billing_policy IS NOT NULL);

CREATE UNIQUE INDEX psps_key_key ON billing.psps USING btree (merchant_id, lower(key)) WHERE (superseded_at IS NULL);
CREATE INDEX psps_environment_rail_created_at_id_idx ON billing.psps USING btree (merchant_id, environment, rail, created_at DESC, id DESC);
CREATE UNIQUE INDEX psps_live_credential_fingerprint_key ON billing.psps USING btree (rail, environment, credential_fingerprint)
    WHERE ((credential_duplicate_at IS NULL) AND (credential_fingerprint IS NOT NULL));
COMMENT ON TABLE billing.psps IS 'Merchant PSP identities. A row is one merchant-owned account on one rail, the target of every payment, subscription and card that names it, with what OpenRails discovers about it; its configuration (settings, credentials, archived) is the PSP document in a file or Vault. The rail vocabulary lives here only; every table that stores rail beside psp_id references (merchant_id, id, rail).';
COMMENT ON COLUMN billing.psps.key IS 'The merchant''s name for the PSP (e.g. mobius): the key of its document. One current PSP holds a key.';
COMMENT ON COLUMN billing.psps.credential_fingerprint IS 'HMAC-SHA256, under a key held in Vault, of the credential naming the gateway account (NMI security_key, Stripe secret_key). Cleared when the PSP archives.';

ALTER TABLE billing.custodians
    DROP COLUMN settings,
    DROP COLUMN credential_versions,
    DROP COLUMN archived;
COMMENT ON TABLE billing.custodians IS 'Merchant custodian identities. A row is one merchant-owned account with a third-party card custodian, the target of the cards it holds; its configuration (settings, credentials, archived) is the custodian document in a file or Vault. Custody is orthogonal to the rail: this says who holds the card, psps says who charges it.';

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
                  'card_attempt_failures',
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
