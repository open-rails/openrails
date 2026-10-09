-- parent: 31 sha256:8b78c6afcb88533bbcc54eac2a3d380092e969f4ef5a766837cab2685afc5de1
-- Stored-credential agreements leave the card row. A mandate is one agreement
-- on one saved card: recurring (one subscription), unscheduled (collection in
-- one currency) or card_on_file (reuse for one-click buys). It holds the
-- network lineage its storing transaction established, scoped to the gateway
-- account that ran it, so a reference is never replayed on another account.
CREATE TABLE billing.mandates (
    id uuid DEFAULT uuidv7() NOT NULL,
    merchant_id uuid NOT NULL,
    customer_id uuid NOT NULL,
    payment_method_id uuid,
    psp_id uuid NOT NULL,
    rail text NOT NULL,
    kind text NOT NULL,
    subscription_id uuid,
    currency text,
    status text NOT NULL,
    end_reason text,
    ended_at timestamp with time zone,
    card_brand text,
    initial_transaction_id text,
    network_transaction_id text,
    transaction_link_id text,
    storing_attempt_id uuid,
    accepted_at timestamp with time zone NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT mandates_pkey PRIMARY KEY (merchant_id, id),
    CONSTRAINT mandates_kind_check CHECK (kind IN ('recurring', 'unscheduled', 'card_on_file')),
    CONSTRAINT mandates_scope_check CHECK ((kind = 'recurring') = (subscription_id IS NOT NULL) AND (kind = 'unscheduled') = (currency IS NOT NULL)),
    CONSTRAINT mandates_currency_check CHECK (currency ~ '^[A-Z0-9]{3,12}$'),
    CONSTRAINT mandates_status_check CHECK (status IN ('active', 'requires_reconsent', 'revoked', 'ended')),
    CONSTRAINT mandates_end_check CHECK ((status IN ('revoked', 'ended')) = (end_reason IS NOT NULL) AND (end_reason IS NULL) = (ended_at IS NULL)),
    CONSTRAINT mandates_end_reason_check CHECK (end_reason IN ('replaced', 'brand_changed', 'closed', 'customer_revoked', 'subscription_ended', 'payment_method_removed') AND (status = 'revoked') = (end_reason = 'customer_revoked')),
    CONSTRAINT mandates_live_method_check CHECK (status IN ('revoked', 'ended') OR payment_method_id IS NOT NULL),
    CONSTRAINT mandates_card_brand_check CHECK (card_brand <> ''),
    CONSTRAINT mandates_initial_transaction_id_check CHECK (initial_transaction_id <> ''),
    CONSTRAINT mandates_network_transaction_id_check CHECK (network_transaction_id <> ''),
    CONSTRAINT mandates_transaction_link_id_check CHECK (length(transaction_link_id) = 22),
    CONSTRAINT mandates_customer_id_fkey FOREIGN KEY (merchant_id, customer_id) REFERENCES billing.customers(merchant_id, id),
    CONSTRAINT mandates_payment_method_id_fkey FOREIGN KEY (merchant_id, customer_id, payment_method_id) REFERENCES billing.payment_methods(merchant_id, customer_id, id) ON DELETE SET NULL (payment_method_id),
    CONSTRAINT mandates_psp_id_rail_fkey FOREIGN KEY (merchant_id, psp_id, rail) REFERENCES billing.psps(merchant_id, id, rail) ON DELETE RESTRICT,
    CONSTRAINT mandates_subscription_id_fkey FOREIGN KEY (merchant_id, customer_id, subscription_id) REFERENCES billing.subscriptions(merchant_id, customer_id, id),
    CONSTRAINT mandates_storing_attempt_id_fkey FOREIGN KEY (merchant_id, storing_attempt_id) REFERENCES billing.payment_attempts(merchant_id, id) ON DELETE SET NULL (storing_attempt_id)
);
COMMENT ON TABLE billing.mandates IS 'A customer''s consent to stored-credential use of one saved card, and the network lineage its storing transaction established: recurring for one subscription, unscheduled for collection in one currency, card_on_file for one-click reuse. Merchant-initiated charges run only under an active mandate and send its references. Retention: permanent, never pruned.';
COMMENT ON COLUMN billing.mandates.payment_method_id IS 'The card the agreement is on. NULL only after an ended or revoked mandate''s card was deleted.';
COMMENT ON COLUMN billing.mandates.psp_id IS 'The gateway account its provider references belong to; a charge through another account never cites them.';
COMMENT ON COLUMN billing.mandates.kind IS 'recurring (one subscription), unscheduled (collection in one currency) or card_on_file (reuse for one-click buys).';
COMMENT ON COLUMN billing.mandates.subscription_id IS 'The subscription a recurring mandate covers.';
COMMENT ON COLUMN billing.mandates.currency IS 'The collection currency an unscheduled mandate covers.';
COMMENT ON COLUMN billing.mandates.status IS 'active; requires_reconsent (merchant-initiated charges wait for fresh consent); revoked (the customer withdrew); ended.';
COMMENT ON COLUMN billing.mandates.end_reason IS 'Why a revoked or ended mandate stopped: replaced, brand_changed, closed, customer_revoked, subscription_ended or payment_method_removed.';
COMMENT ON COLUMN billing.mandates.card_brand IS 'The card brand the agreement was established on.';
COMMENT ON COLUMN billing.mandates.initial_transaction_id IS 'The provider''s id of the storing transaction (NMI transactionid, Stripe pi_ or seti_). NULL until one is approved; written once.';
COMMENT ON COLUMN billing.mandates.network_transaction_id IS 'The scheme transaction id (Visa TID, Mastercard Trace ID) of the storing transaction, where the provider returns it; written once.';
COMMENT ON COLUMN billing.mandates.transaction_link_id IS 'The Mastercard Transaction Link ID of the storing transaction, where the provider returns it; written once.';
COMMENT ON COLUMN billing.mandates.storing_attempt_id IS 'The payment attempt of the storing transaction, while it is retained.';
COMMENT ON COLUMN billing.mandates.accepted_at IS 'When the customer gave the consent.';

CREATE UNIQUE INDEX mandates_recurring_live_key ON billing.mandates (merchant_id, subscription_id) WHERE kind = 'recurring' AND status IN ('active', 'requires_reconsent');
CREATE UNIQUE INDEX mandates_unscheduled_live_key ON billing.mandates (merchant_id, customer_id, currency) WHERE kind = 'unscheduled' AND status IN ('active', 'requires_reconsent');
CREATE UNIQUE INDEX mandates_card_on_file_live_key ON billing.mandates (merchant_id, payment_method_id, psp_id) WHERE kind = 'card_on_file' AND status IN ('active', 'requires_reconsent');
CREATE INDEX mandates_payment_method_id_idx ON billing.mandates (merchant_id, payment_method_id, psp_id) WHERE payment_method_id IS NOT NULL;
CREATE INDEX mandates_customer_id_created_at_id_idx ON billing.mandates (merchant_id, customer_id, created_at DESC, id DESC);

-- The references a storing transaction established never change, and a
-- revoked or ended mandate never comes back.
CREATE FUNCTION billing.guard_mandate_update() RETURNS trigger
    LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
BEGIN
    IF (to_jsonb(NEW) - ARRAY['payment_method_id', 'status', 'end_reason', 'ended_at', 'initial_transaction_id', 'network_transaction_id', 'transaction_link_id', 'storing_attempt_id', 'updated_at'])
       IS DISTINCT FROM (to_jsonb(OLD) - ARRAY['payment_method_id', 'status', 'end_reason', 'ended_at', 'initial_transaction_id', 'network_transaction_id', 'transaction_link_id', 'storing_attempt_id', 'updated_at'])
       OR (OLD.payment_method_id IS NOT NULL AND NEW.payment_method_id IS NOT NULL AND NEW.payment_method_id <> OLD.payment_method_id)
       OR (OLD.initial_transaction_id IS NOT NULL AND NEW.initial_transaction_id IS DISTINCT FROM OLD.initial_transaction_id)
       OR (OLD.network_transaction_id IS NOT NULL AND NEW.network_transaction_id IS DISTINCT FROM OLD.network_transaction_id)
       OR (OLD.transaction_link_id IS NOT NULL AND NEW.transaction_link_id IS DISTINCT FROM OLD.transaction_link_id)
       OR (OLD.status IN ('revoked', 'ended') AND (NEW.status, NEW.end_reason, NEW.ended_at) IS DISTINCT FROM (OLD.status, OLD.end_reason, OLD.ended_at)) THEN
        RAISE EXCEPTION 'mandate % changes what it recorded', OLD.id USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER guard_mandate_update BEFORE UPDATE ON billing.mandates FOR EACH ROW EXECUTE FUNCTION billing.guard_mandate_update();

-- Backfill. A reference is the storing transaction's; the gateway account is
-- the one its attempt was answered by, else the account that holds the card.

-- Each subscription's recurring agreement, on the account it charges through.
INSERT INTO billing.mandates (merchant_id, customer_id, payment_method_id, psp_id, rail, kind, subscription_id, status, card_brand, initial_transaction_id, storing_attempt_id, accepted_at)
SELECT s.merchant_id, s.customer_id, pm.id, s.psp_id, s.rail, 'recurring', s.id, 'active', NULLIF(pm.card_brand, ''), pm.stored_credential_recurring_ref,
       (SELECT a.id FROM billing.payment_attempts a
         WHERE a.merchant_id = pm.merchant_id AND a.psp_id = s.psp_id AND a.transaction_id = pm.stored_credential_recurring_ref
         ORDER BY a.attempted_at, a.id LIMIT 1),
       s.created_at
FROM billing.subscriptions s
JOIN billing.payment_methods pm ON pm.merchant_id = s.merchant_id AND pm.customer_id = s.customer_id AND pm.id = s.payment_method_id
WHERE s.deleted_at IS NULL AND pm.stored_credential_recurring_ref IS NOT NULL AND pm.rail = s.rail
  AND (pm.custodian <> 'psp' OR pm.psp_id = s.psp_id);

-- Each collection default's unscheduled agreement.
INSERT INTO billing.mandates (merchant_id, customer_id, payment_method_id, psp_id, rail, kind, currency, status, card_brand, initial_transaction_id, storing_attempt_id, accepted_at)
SELECT ms.merchant_id, ms.customer_id, pm.id, p.id, p.rail, 'unscheduled', ms.currency, 'active', NULLIF(pm.card_brand, ''), pm.stored_credential_unscheduled_ref, a.id, ms.updated_at
FROM billing.money_settings ms
JOIN billing.payment_methods pm ON pm.merchant_id = ms.merchant_id AND pm.customer_id = ms.customer_id AND pm.id = ms.collection_payment_method_id
LEFT JOIN LATERAL (
    SELECT a.id, a.psp_id FROM billing.payment_attempts a
    WHERE a.merchant_id = pm.merchant_id AND a.payment_method_id = pm.id AND a.transaction_id = pm.stored_credential_unscheduled_ref
    ORDER BY a.attempted_at, a.id LIMIT 1) a ON true
JOIN billing.psps p ON p.merchant_id = pm.merchant_id AND p.id = COALESCE(a.psp_id, pm.psp_id) AND p.rail = pm.rail
WHERE pm.stored_credential_unscheduled_ref IS NOT NULL;

-- Every card kept for reuse: its unscheduled lineage, or on Stripe the
-- off-session setup that stored it.
INSERT INTO billing.mandates (merchant_id, customer_id, payment_method_id, psp_id, rail, kind, status, card_brand, initial_transaction_id, storing_attempt_id, accepted_at)
SELECT pm.merchant_id, pm.customer_id, pm.id, p.id, p.rail, 'card_on_file', 'active', NULLIF(pm.card_brand, ''), l.ref, a.id, pm.created_at
FROM billing.payment_methods pm
CROSS JOIN LATERAL (SELECT COALESCE(pm.stored_credential_unscheduled_ref, CASE WHEN pm.rail = 'stripe' THEN pm.stored_credential_recurring_ref END) AS ref) l
LEFT JOIN LATERAL (
    SELECT a.id, a.psp_id FROM billing.payment_attempts a
    WHERE a.merchant_id = pm.merchant_id AND a.payment_method_id = pm.id AND a.transaction_id = l.ref
    ORDER BY a.attempted_at, a.id LIMIT 1) a ON true
JOIN billing.psps p ON p.merchant_id = pm.merchant_id AND p.id = COALESCE(a.psp_id, pm.psp_id) AND p.rail = pm.rail
WHERE l.ref IS NOT NULL AND (pm.park_reason IS NULL OR pm.park_reason NOT LIKE 'delete:%');

ALTER TABLE billing.payment_methods DROP COLUMN stored_credential_recurring_ref, DROP COLUMN stored_credential_unscheduled_ref;

-- Repair: none-needed both columns are new and NULL on every existing attempt.
ALTER TABLE billing.payment_attempts ADD COLUMN mandate_id uuid, ADD COLUMN sent_initial_transaction_id text;
ALTER TABLE ONLY billing.payment_attempts
    ADD CONSTRAINT payment_attempts_sent_initial_transaction_id_check CHECK (sent_initial_transaction_id <> '');
ALTER TABLE ONLY billing.payment_attempts
    ADD CONSTRAINT payment_attempts_mandate_id_fkey FOREIGN KEY (merchant_id, mandate_id) REFERENCES billing.mandates(merchant_id, id) ON DELETE SET NULL (mandate_id);
COMMENT ON COLUMN billing.payment_attempts.mandate_id IS 'The mandate whose references the attempt sent: a merchant-initiated charge, or a customer-present one on a stored credential.';
COMMENT ON COLUMN billing.payment_attempts.sent_initial_transaction_id IS 'The initial transaction id the attempt sent the provider, verbatim.';

-- Mandates are merchant rows and occupy a restore destination.
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
