-- Operations history: what OpenRails did to the outside world, one entry per
-- phase of each provider operation, on the history envelope change_log
-- carries. provider_mutation_logs folds into it, and price_key_movements into
-- change_log as price-key moves; both keep their ids.

-- The destructive-volume breaker's fact: the attempt it admitted to the
-- provider, written under its lock. It counted the attempting rows of the
-- mutation log before.
ALTER TABLE billing.provider_intents ADD COLUMN sent_attempt integer CHECK (sent_attempt >= 1);
COMMENT ON COLUMN billing.provider_intents.sent_attempt IS 'The attempt the destructive-volume breaker last admitted to the provider, set under its lock before the call. An in_flight intent counts against the breaker''s budget only while this is its current attempt: a claim alone sends nothing.';
UPDATE billing.provider_intents i SET sent_attempt = i.attempts
WHERE i.status = 'in_flight' AND EXISTS (
    SELECT 1 FROM billing.provider_mutation_logs l
    WHERE l.merchant_id = i.merchant_id AND l.provider_intent_id = i.id AND l.phase = 'attempting' AND l.attempt = i.attempts);

CREATE TABLE billing.operation_log (
    merchant_id uuid NOT NULL REFERENCES billing.merchants (id) ON DELETE RESTRICT,
    id uuid NOT NULL DEFAULT uuidv7(),
    occurred_at timestamptz NOT NULL DEFAULT now(),
    actor text NOT NULL CHECK (octet_length(actor) <= 4200 AND (actor ~ '^(staff|customer):.' OR actor ~ '^provider:[a-z][a-z0-9_]{0,31}$' OR actor IN ('system', 'file', 'vault'))),
    source text NOT NULL CHECK (source IN ('api', 'customer', 'console', 'catalog_apply', 'cli', 'reload', 'job', 'intent', 'webhook', 'provider_read')),
    object_type text NOT NULL CHECK (object_type IN ('provider_intent')),
    object_id text NOT NULL CHECK (octet_length(object_id) BETWEEN 1 AND 512),
    action text NOT NULL CHECK (action IN ('attempting', 'succeeded', 'failed', 'unknown', 'parked')),
    changes jsonb NOT NULL CHECK (jsonb_typeof(changes) = 'object' AND jsonb_typeof(changes->'version') = 'number' AND octet_length(changes::text) <= 262144),
    reason text CHECK (octet_length(reason) BETWEEN 1 AND 1024),
    request_id text CHECK (octet_length(request_id) BETWEEN 1 AND 128),
    operation_id uuid,
    rail text NOT NULL,
    psp_id uuid,
    custodian_id uuid,
    subscription_id uuid,
    PRIMARY KEY (merchant_id, id),
    CONSTRAINT operation_log_custodian_id_fkey FOREIGN KEY (merchant_id, custodian_id) REFERENCES billing.custodians (merchant_id, id) ON DELETE RESTRICT,
    CONSTRAINT operation_log_psp_id_rail_fkey FOREIGN KEY (merchant_id, psp_id, rail) REFERENCES billing.psps (merchant_id, id, rail) ON DELETE RESTRICT,
    CONSTRAINT operation_log_addressed_check CHECK (psp_id IS NOT NULL OR custodian_id IS NOT NULL)
);
COMMENT ON TABLE billing.operation_log IS 'Operations history: each phase of each provider operation OpenRails ran (attempting before the provider call, then its result or a hold), who wanted it and where it was sent. Attempting and result entries are written ahead, on their own, and never rolled back; a hold commits with the intent''s transition. Append-only: only retention and a merchant purge delete. Retention: rows are deleted 25 months (761 days) after occurred_at.';
COMMENT ON COLUMN billing.operation_log.actor IS 'Who wanted the operation: staff:<invoker>, customer:<invoker>, provider:<rail>, or system.';
COMMENT ON COLUMN billing.operation_log.object_id IS 'The provider intent''s id; an entry whose intent was never recorded names itself.';
COMMENT ON COLUMN billing.operation_log.changes IS 'Version 1: the intent type (kind), attempt, origin, executor, idempotency key, the outcome''s scrubbed reason, a hold''s reason_code and scrubbed evidence. Never credentials, card data or provider bodies.';
COMMENT ON COLUMN billing.operation_log.operation_id IS 'The provider intent the entry belongs to; kept after retention deletes the intent.';
COMMENT ON COLUMN billing.operation_log.psp_id IS 'The PSP the operation was sent to. Required unless it was sent to a custodian (operation_log_addressed_check).';
COMMENT ON COLUMN billing.operation_log.custodian_id IS 'The custodian the operation was sent to, for custodian-addressed intents.';
COMMENT ON COLUMN billing.operation_log.subscription_id IS 'The subscription the operation acted on, for a subscription''s history.';

CREATE INDEX operation_log_occurred_at_idx ON billing.operation_log (merchant_id, occurred_at);
CREATE INDEX operation_log_operation_id_idx ON billing.operation_log (merchant_id, operation_id, occurred_at DESC, id DESC);
CREATE INDEX operation_log_subscription_id_idx ON billing.operation_log (merchant_id, subscription_id, occurred_at DESC, id DESC)
    WHERE subscription_id IS NOT NULL;
CREATE INDEX operation_log_psp_id_idx ON billing.operation_log (merchant_id, psp_id)
    WHERE psp_id IS NOT NULL;
CREATE INDEX operation_log_custodian_id_idx ON billing.operation_log (merchant_id, custodian_id)
    WHERE custodian_id IS NOT NULL;

CREATE TRIGGER immutable_operation_log BEFORE UPDATE ON billing.operation_log
    FOR EACH ROW EXECUTE FUNCTION billing.reject_immutable_billing_fact();
CREATE TRIGGER immutable_operation_log_truncate BEFORE TRUNCATE ON billing.operation_log
    FOR EACH STATEMENT EXECUTE FUNCTION billing.reject_immutable_billing_fact();

-- Each mutation log row becomes one entry with its id. A row from before the
-- log named its subscription, origin or actor takes them from its intent. A
-- row too large for the log keeps the start of its reason and of its
-- evidence's JSON, and an actor over 4200 bytes its first 1024 characters;
-- either says truncated.
INSERT INTO billing.operation_log (merchant_id, id, occurred_at, actor, source, object_type, object_id, action, changes, operation_id, rail, psp_id, custodian_id, subscription_id)
SELECT l.merchant_id, l.id, l.created_at,
    CASE WHEN octet_length(w.actor) <= 4200 THEN w.actor ELSE left(w.actor, 1024) END,
    'intent', 'provider_intent', COALESCE(l.operation_id, l.provider_intent_id, l.id)::text, l.phase,
    CASE
        WHEN octet_length(w.payload::text) > 262144 THEN (w.payload - 'reason' - 'evidence')
            || jsonb_strip_nulls(jsonb_build_object('reason', left(l.reason, 8192), 'evidence_excerpt', left(l.evidence::text, 16384), 'truncated', true))
        WHEN octet_length(w.actor) > 4200 THEN w.payload || '{"truncated": true}'::jsonb
        ELSE w.payload
    END,
    COALESCE(l.operation_id, l.provider_intent_id), l.rail, l.psp_id, l.custodian_id, COALESCE(l.subscription_id, i.subscription_id)
FROM billing.provider_mutation_logs l
LEFT JOIN billing.provider_intents i ON i.merchant_id = l.merchant_id AND i.id = COALESCE(l.operation_id, l.provider_intent_id)
CROSS JOIN LATERAL (SELECT
    CASE
        WHEN COALESCE(l.origin, i.origin) = 'admin' AND COALESCE(l.actor, i.actor) <> '' THEN 'staff:' || COALESCE(l.actor, i.actor)
        WHEN COALESCE(l.origin, i.origin) = 'user' AND COALESCE(l.actor, i.actor) <> '' THEN 'customer:' || COALESCE(l.actor, i.actor)
        WHEN COALESCE(l.origin, i.origin) = 'provider' THEN 'provider:' || l.rail
        ELSE 'system'
    END AS actor,
    (SELECT jsonb_object_agg(e.key, e.value) FROM jsonb_each(jsonb_build_object(
        'version', 1, 'kind', COALESCE(l.intent_type, i.intent_type), 'attempt', l.attempt, 'origin', COALESCE(l.origin, i.origin),
        'executor', l.executor, 'idempotency_key', l.idempotency_key, 'reason', l.reason, 'reason_code', l.reason_code,
        'evidence', l.evidence)) e WHERE e.value <> 'null'::jsonb) AS payload) w;

DROP TABLE billing.provider_mutation_logs;

-- A price key's moves are configuration history: from occurred_at the key
-- named price_id or, archived, nothing. The catalog writer records them with
-- the change that moved the key; the triggers that wrote the old log go.
ALTER TABLE billing.change_log DROP CONSTRAINT change_log_action_check;
ALTER TABLE billing.change_log ADD CONSTRAINT change_log_action_check CHECK (action IN ('create', 'update', 'archive', 'unarchive', 'delete', 'key_add', 'key_remove', 'key_rename', 'move'));

-- Who moved a key was never recorded: a folded move is the system's.
INSERT INTO billing.change_log (merchant_id, id, occurred_at, actor, source, object_type, object_id, action, changes)
SELECT m.merchant_id, m.id, m.effective_at, 'system', 'job', 'price', product.key || ':' || m.key, 'move',
    jsonb_build_object('version', 1, 'price_id', p.public_id, 'archived', m.archived)
FROM billing.price_key_movements m
JOIN billing.prices p ON p.merchant_id = m.merchant_id AND p.id = m.price_id
JOIN billing.products product ON product.merchant_id = p.merchant_id AND product.id = p.product_id;

DROP TRIGGER catalog_price_retired ON billing.prices;
DROP FUNCTION billing.catalog_price_retired();
DROP TRIGGER catalog_product_offer_state ON billing.products;
DROP FUNCTION billing.catalog_product_offer_state();
DROP TABLE billing.price_key_movements;

-- A billing restore's destination holds no operation history.
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
                  'carts',
                  'catalog_applications',
                  'catalog_field_owners',
                  'catalog_limits',
                  'catalog_meters',
                  'catalog_product_limits',
                  'catalog_rate_cards',
                  'catalog_restore_receipts',
                  'custodians',
                  'customer_delinquency',
                  'customer_invoice_profiles',
                  'customer_limit_overrides',
                  'customer_low_balance_lines',
                  'customer_messages',
                  'customers',
                  'dashboard_configs',
                  'destructive_actions',
                  'destructive_holds',
                  'destructive_run_before_images',
                  'eventlog_events',
                  'failed_usage_windows',
                  'grants',
                  'host_outbox',
                  'idempotency_keys',
                  'leases',
                  'ledger_accounts',
                  'ledger_transfers',
                  'limit_adjustments',
                  'limit_hold_parts',
                  'limit_holds',
                  'limit_windows',
                  'maintenance_runs',
                  'mandates',
                  'merchant_destructive_policy',
                  'meter_period_quantities',
                  'money_settings',
                  'nmi_bulk_checkpoints',
                  'nmi_history_months',
                  'nmi_history_reads',
                  'notification_events',
                  'operation_log',
                  'order_attempts',
                  'order_lines',
                  'orders',
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
                  'provider_schedules',
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
                  'stored_card_checks',
                  'subscription_status_transitions',
                  'subscription_billing_changes',
                  'subscription_verifications',
                  'subscriptions',
                  'usage_accruals',
                  'usage_daily',
                  'usage_minutes',
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
    END IF;
    RETURN NEW;
END;
$_$;
