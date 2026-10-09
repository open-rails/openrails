-- parent: 33 sha256:444239f9f32989a8c8f5f7c2ae9b3db0925743f624885abdf25386a58fb0e6ab
-- Reprice batches and plan migrations become one resource, price_migrations.
-- A subscription's one pending change (a migration's move, a scheduled
-- downgrade, a deferred seat change) becomes one scheduled_changes row, which
-- absorbs subscription_reprices and subscriptions.scheduled_price_id.
CREATE TABLE billing.price_migrations (
    merchant_id uuid NOT NULL,
    id uuid DEFAULT uuidv7() NOT NULL,
    from_price_id uuid,
    from_product_id uuid,
    from_price_key text,
    to_price_id uuid NOT NULL,
    effective_at timestamp with time zone NOT NULL,
    fallback_policy text NOT NULL,
    subscriptions_matched integer NOT NULL,
    subscriptions_skipped integer NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    canceled_at timestamp with time zone,
    CONSTRAINT price_migrations_source_check CHECK (((from_price_id IS NULL) = (from_price_key IS NOT NULL)) AND ((from_product_id IS NULL) = (from_price_key IS NULL))),
    CONSTRAINT price_migrations_from_price_key_check CHECK (from_price_key <> ''),
    CONSTRAINT price_migrations_fallback_policy_check CHECK (fallback_policy IN ('keep_grandfathered', 'cancel_at_period_end')),
    CONSTRAINT price_migrations_counts_check CHECK (subscriptions_matched >= 0 AND subscriptions_skipped BETWEEN 0 AND subscriptions_matched)
);
COMMENT ON TABLE billing.price_migrations IS 'One move of subscribers from a price (from_price_id), or from every version of a price key but the target (from_product_id, from_price_key), to to_price_id at each subscription''s first renewal on or after effective_at. Matched and skipped are facts of creation (a skipped subscription gets no scheduled change); progress is counted from the scheduled_changes that carry price_migration_id. Retention: permanent, never pruned.';
COMMENT ON COLUMN billing.price_migrations.fallback_policy IS 'The operator''s choice for subscriptions whose provider cannot be moved from OpenRails (CCBill, Solana, an unreachable NMI schedule): keep_grandfathered leaves them on the old price; cancel_at_period_end records that they should end, which OpenRails does not do.';
COMMENT ON COLUMN billing.price_migrations.canceled_at IS 'When its still-scheduled changes were canceled; a canceled migration is never re-driven.';

ALTER TABLE ONLY billing.price_migrations
    ADD CONSTRAINT price_migrations_pkey PRIMARY KEY (merchant_id, id);
ALTER TABLE ONLY billing.price_migrations
    ADD CONSTRAINT price_migrations_merchant_id_fkey FOREIGN KEY (merchant_id) REFERENCES billing.merchants(id) ON DELETE RESTRICT;
ALTER TABLE ONLY billing.price_migrations
    ADD CONSTRAINT price_migrations_from_price_id_fkey FOREIGN KEY (merchant_id, from_price_id) REFERENCES billing.prices(merchant_id, id) ON DELETE RESTRICT;
ALTER TABLE ONLY billing.price_migrations
    ADD CONSTRAINT price_migrations_from_product_id_fkey FOREIGN KEY (merchant_id, from_product_id) REFERENCES billing.products(merchant_id, id) ON DELETE RESTRICT;
ALTER TABLE ONLY billing.price_migrations
    ADD CONSTRAINT price_migrations_to_price_id_fkey FOREIGN KEY (merchant_id, to_price_id) REFERENCES billing.prices(merchant_id, id) ON DELETE RESTRICT;
CREATE INDEX price_migrations_created_at_id_idx ON billing.price_migrations USING btree (merchant_id, created_at DESC, id DESC);
CREATE INDEX price_migrations_from_key_created_at_id_idx ON billing.price_migrations USING btree (merchant_id, from_product_id, from_price_key, created_at DESC, id DESC) WHERE (from_price_key IS NOT NULL);
CREATE INDEX price_migrations_from_price_id_idx ON billing.price_migrations USING btree (merchant_id, from_price_id) WHERE (from_price_id IS NOT NULL);
CREATE INDEX price_migrations_to_price_id_idx ON billing.price_migrations USING btree (merchant_id, to_price_id);

CREATE TABLE billing.scheduled_changes (
    merchant_id uuid NOT NULL,
    id uuid DEFAULT uuidv7() NOT NULL,
    subscription_id uuid NOT NULL,
    from_price_id uuid NOT NULL,
    price_id uuid NOT NULL,
    quantity integer,
    effective_at timestamp with time zone NOT NULL,
    source text NOT NULL,
    price_migration_id uuid,
    status text NOT NULL,
    blocked_reason text,
    acknowledged_short_notice boolean NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    applied_at timestamp with time zone,
    canceled_at timestamp with time zone,
    CONSTRAINT scheduled_changes_source_check CHECK (source IN ('change', 'migration') AND ((source = 'migration') = (price_migration_id IS NOT NULL))),
    CONSTRAINT scheduled_changes_status_check CHECK (status IN ('scheduled', 'applied', 'canceled', 'blocked')),
    CONSTRAINT scheduled_changes_quantity_check CHECK (quantity IS NULL OR quantity >= 1),
    CONSTRAINT scheduled_changes_applied_check CHECK ((status = 'applied') = (applied_at IS NOT NULL)),
    CONSTRAINT scheduled_changes_canceled_check CHECK ((status = 'canceled') = (canceled_at IS NOT NULL)),
    CONSTRAINT scheduled_changes_blocked_check CHECK ((status = 'blocked') = (blocked_reason IS NOT NULL) AND (status <> 'blocked' OR source = 'migration') AND blocked_reason <> '')
);
COMMENT ON TABLE billing.scheduled_changes IS 'A subscription''s change waiting for its renewal: the price (price_id) and, for a per-seat price, the seats (quantity; NULL keeps the subscription''s) it bills from its first renewal on or after effective_at. At most one is scheduled per subscription. source is change (a tier or seat change) or migration (price_migration_id). blocked is a migration''s move its provider could not take; blocked_reason says why, and a push failure (rail_push_failed:) is re-driven. Retention: permanent, never pruned.';
COMMENT ON COLUMN billing.scheduled_changes.from_price_id IS 'The price the subscription billed when the change was scheduled; the change applies only while it still does.';
COMMENT ON COLUMN billing.scheduled_changes.acknowledged_short_notice IS 'A price increase scheduled inside the merchant''s notice window under the request''s acknowledge_short_notice.';

ALTER TABLE ONLY billing.scheduled_changes
    ADD CONSTRAINT scheduled_changes_pkey PRIMARY KEY (merchant_id, id);
ALTER TABLE ONLY billing.scheduled_changes
    ADD CONSTRAINT scheduled_changes_merchant_id_fkey FOREIGN KEY (merchant_id) REFERENCES billing.merchants(id) ON DELETE RESTRICT;
ALTER TABLE ONLY billing.scheduled_changes
    ADD CONSTRAINT scheduled_changes_subscription_id_fkey FOREIGN KEY (merchant_id, subscription_id) REFERENCES billing.subscriptions(merchant_id, id) ON DELETE RESTRICT;
ALTER TABLE ONLY billing.scheduled_changes
    ADD CONSTRAINT scheduled_changes_from_price_id_fkey FOREIGN KEY (merchant_id, from_price_id) REFERENCES billing.prices(merchant_id, id) ON DELETE RESTRICT;
ALTER TABLE ONLY billing.scheduled_changes
    ADD CONSTRAINT scheduled_changes_price_id_fkey FOREIGN KEY (merchant_id, price_id) REFERENCES billing.prices(merchant_id, id) ON DELETE RESTRICT;
ALTER TABLE ONLY billing.scheduled_changes
    ADD CONSTRAINT scheduled_changes_price_migration_id_fkey FOREIGN KEY (merchant_id, price_migration_id) REFERENCES billing.price_migrations(merchant_id, id) ON DELETE RESTRICT;
CREATE UNIQUE INDEX scheduled_changes_subscription_id_key ON billing.scheduled_changes USING btree (merchant_id, subscription_id) WHERE (status = 'scheduled');
CREATE INDEX scheduled_changes_subscription_id_idx ON billing.scheduled_changes USING btree (merchant_id, subscription_id);
CREATE INDEX scheduled_changes_price_migration_id_idx ON billing.scheduled_changes USING btree (merchant_id, price_migration_id) WHERE (price_migration_id IS NOT NULL);
CREATE INDEX scheduled_changes_from_price_id_idx ON billing.scheduled_changes USING btree (merchant_id, from_price_id);
CREATE INDEX scheduled_changes_price_id_idx ON billing.scheduled_changes USING btree (merchant_id, price_id);
CREATE INDEX scheduled_changes_redrive_idx ON billing.scheduled_changes USING btree (merchant_id, created_at) WHERE (status = 'blocked' AND blocked_reason LIKE 'rail_push_failed:%');

-- Every batch, reprice and scheduled downgrade carries over with its id.
INSERT INTO billing.price_migrations (merchant_id, id, from_price_id, from_product_id, from_price_key, to_price_id, effective_at, fallback_policy, subscriptions_matched, subscriptions_skipped, created_at)
SELECT b.merchant_id, b.id,
       CASE WHEN b.kind = 'plan_change' THEN b.source_price_id END,
       CASE WHEN b.kind = 'reprice' THEN p.product_id END,
       CASE WHEN b.kind = 'reprice' THEN b.price_key END,
       b.to_price_id, b.effective_at, COALESCE(b.fallback_policy, 'keep_grandfathered'),
       b.subscriptions_matched, b.subscriptions_skipped, b.created_at
FROM billing.reprice_batches b
JOIN billing.prices p ON p.merchant_id = b.merchant_id AND p.id = b.to_price_id;

INSERT INTO billing.scheduled_changes (merchant_id, id, subscription_id, from_price_id, price_id, effective_at, source, price_migration_id, status, blocked_reason, acknowledged_short_notice, created_at, applied_at, canceled_at)
SELECT r.merchant_id, r.id, r.subscription_id, r.from_price_id, r.to_price_id, r.effective_at,
       CASE WHEN r.reprice_batch_id IS NULL THEN 'change' ELSE 'migration' END, r.reprice_batch_id,
       r.status, r.blocked_reason, r.acknowledged_short_notice, r.created_at, r.applied_at, r.canceled_at
FROM billing.subscription_reprices r;

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM billing.subscriptions s
               JOIN billing.scheduled_changes c ON c.merchant_id = s.merchant_id AND c.subscription_id = s.id AND c.status = 'scheduled'
               WHERE s.scheduled_price_id IS NOT NULL) THEN
        RAISE EXCEPTION 'a subscription holds both a scheduled reprice and a scheduled downgrade; cancel one first' USING ERRCODE = '23505';
    END IF;
END $$;

-- A scheduled downgrade applies at the next renewal: any instant before it.
INSERT INTO billing.scheduled_changes (merchant_id, subscription_id, from_price_id, price_id, effective_at, source, status, acknowledged_short_notice, created_at)
SELECT s.merchant_id, s.id, s.price_id, s.scheduled_price_id, s.updated_at, 'change', 'scheduled', false, s.updated_at
FROM billing.subscriptions s
WHERE s.scheduled_price_id IS NOT NULL;

DROP TABLE billing.subscription_reprices;
DROP TABLE billing.reprice_batches;
ALTER TABLE billing.subscriptions DROP COLUMN scheduled_price_id;

CREATE OR REPLACE FUNCTION billing.products_guard_tier_group() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF NEW.tier_group IS DISTINCT FROM OLD.tier_group AND EXISTS (
        SELECT 1 FROM billing.subscriptions s
        WHERE s.merchant_id = OLD.merchant_id AND s.product_id = OLD.id
          AND s.deleted_at IS NULL
          AND s.status IN ('active', 'pending', 'past_due', 'awaiting_method', 'unverified')
          AND (EXISTS (
                SELECT 1 FROM billing.scheduled_changes c
                WHERE c.merchant_id = s.merchant_id AND c.subscription_id = s.id AND c.status = 'scheduled')
            OR EXISTS (
                SELECT 1 FROM billing.provider_intents i
                WHERE i.merchant_id = s.merchant_id AND i.subscription_id = s.id
                  AND i.intent_type IN ('nmi_upgrade', 'stripe_tier_change', 'initial_membership')
                  AND i.status IN ('pending', 'in_flight', 'unknown_needs_verify', 'failed_retryable')))
    ) THEN
        RAISE EXCEPTION 'product tier group cannot change while a live subscription has a plan change in flight'
            USING ERRCODE = '23514', CONSTRAINT = 'products_live_subscription_tier_group';
    END IF;
    RETURN NEW;
END;
$$;

-- Price migrations and scheduled changes replace reprice batches and reprices.
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
