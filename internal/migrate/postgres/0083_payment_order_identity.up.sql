-- Repair: none-needed Prelaunch cut: historical price-only payments are not reconstructed. Such a database must be reset or explicitly rebuilt before this schema is adopted; this migration never deletes financial rows.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM billing.payments WHERE order_id IS NULL) THEN
        RAISE EXCEPTION 'price-only payment history cannot be upgraded without reconstruction; use a fresh prelaunch billing schema or an explicitly rebuilt order book' USING ERRCODE = '55000';
    END IF;
END;
$$;
ALTER TABLE billing.payments DROP CONSTRAINT payments_payable_check;
ALTER TABLE billing.payments DROP COLUMN price_id;
ALTER TABLE billing.payments ADD CONSTRAINT payments_order_required CHECK (order_id IS NOT NULL);
COMMENT ON COLUMN billing.payments.order_id IS 'Required immutable bill identity for every payment and reversal. Prices and products belong to the order lines.';

-- Legacy key windows convert through the payment's order lines: a purchase
-- takes the line product that grants the key, a subscription any product its
-- payments' orders bought.
CREATE OR REPLACE FUNCTION billing.convert_entitlement_windows(p_merchant uuid, p_source regclass, p_at timestamptz)
RETURNS bigint LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $fn$
DECLARE converted bigint;
BEGIN
    DROP TABLE IF EXISTS pg_temp.access_conversion;
    EXECUTE format($q$
    CREATE TEMP TABLE access_conversion ON COMMIT DROP AS
    WITH win AS (
        SELECT e.customer_id, e.entitlement::text COLLATE "C" AS entitlement, e.starts_at, e.ends_at,
               e.revoked_at, e.revoke_reason, e.destructive_run_id::text AS destructive_run_id,
               g.source_type, COALESCE(g.source_id, g.id::text) AS source_id,
               CASE WHEN g.source_id ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' THEN g.source_id::uuid END AS source_uuid
        FROM %1$s e
        JOIN grants g ON g.merchant_id = e.merchant_id AND g.customer_id = e.customer_id AND g.id = e.grant_id
        WHERE e.merchant_id = %2$L AND e.deleted_at IS NULL
    ), purchases AS (
        SELECT w.customer_id, line.product_id, 'purchase'::text AS source_type, w.source_id, pay.id AS payment_id,
               min(w.starts_at) AS starts_at,
               CASE WHEN bool_or(w.ends_at IS NULL) THEN NULL ELSE max(w.ends_at) END AS ends_at,
               bool_and(w.revoked_at IS NOT NULL) AS revoked, max(w.revoked_at) AS revoked_at, max(w.revoke_reason) AS revoke_reason,
               max(w.destructive_run_id) AS destructive_run_id
        FROM win w
        JOIN payments pay ON pay.merchant_id = %2$L AND pay.id = w.source_uuid AND pay.customer_id = w.customer_id
        CROSS JOIN LATERAL (
            SELECT l.product_id FROM order_lines l
            WHERE l.merchant_id = %2$L AND l.order_id = pay.order_id AND l.product_id IS NOT NULL
            ORDER BY (SELECT count(*) FROM product_entitlements pe
                      WHERE pe.merchant_id = %2$L AND pe.product_id = l.product_id AND pe.removed_at IS NULL
                        AND pe.entitlement = w.entitlement) DESC, l.position
            LIMIT 1
        ) line
        WHERE w.source_type = 'purchase'
        GROUP BY w.customer_id, line.product_id, w.source_id, pay.id
    ), periods AS (
        SELECT w.customer_id, w.source_type, w.source_id, w.source_uuid, w.starts_at,
               CASE WHEN bool_or(w.ends_at IS NULL) THEN NULL ELSE max(w.ends_at) END AS ends_at,
               bool_and(w.revoked_at IS NOT NULL) AS revoked, max(w.revoked_at) AS revoked_at, max(w.revoke_reason) AS revoke_reason,
               max(w.destructive_run_id) AS destructive_run_id, array_agg(DISTINCT w.entitlement) AS keys
        FROM win w WHERE w.source_type IN ('subscription', 'grace')
        GROUP BY w.customer_id, w.source_type, w.source_id, w.source_uuid, w.starts_at
    ), subscription_windows AS (
        SELECT p.customer_id, best.product_id, p.source_type, p.source_id, NULL::uuid AS payment_id, p.starts_at, p.ends_at,
               p.revoked, p.revoked_at, p.revoke_reason, p.destructive_run_id
        FROM periods p
        JOIN subscriptions s ON s.merchant_id = %2$L AND s.id = p.source_uuid
        CROSS JOIN LATERAL (
            SELECT c.product_id FROM (
                SELECT s.product_id
                UNION SELECT l.product_id FROM payments pay
                JOIN order_lines l ON l.merchant_id = pay.merchant_id AND l.order_id = pay.order_id
                WHERE pay.merchant_id = %2$L AND pay.subscription_id = s.id AND l.product_id IS NOT NULL
            ) c
            ORDER BY (SELECT count(*) FROM product_entitlements pe
                      WHERE pe.merchant_id = %2$L AND pe.product_id = c.product_id AND pe.removed_at IS NULL
                        AND pe.entitlement = ANY (p.keys)) DESC,
                     (c.product_id = s.product_id) DESC, c.product_id
            LIMIT 1
        ) best
    ), admin_groups AS (
        SELECT w.customer_id, w.source_id, w.starts_at,
               CASE WHEN bool_or(w.ends_at IS NULL) THEN NULL ELSE max(w.ends_at) END AS ends_at,
               bool_and(w.revoked_at IS NOT NULL) AS revoked, max(w.revoked_at) AS revoked_at, max(w.revoke_reason) AS revoke_reason,
               max(w.destructive_run_id) AS destructive_run_id, array_agg(DISTINCT w.entitlement ORDER BY w.entitlement) AS keys
        FROM win w WHERE w.source_type = 'admin'
        GROUP BY w.customer_id, w.source_id, w.starts_at
    ), admin_windows AS (
        SELECT a.customer_id, match.product_id, 'grant'::text AS source_type, a.source_id, NULL::uuid AS payment_id, a.starts_at, a.ends_at,
               a.revoked, a.revoked_at, a.revoke_reason, a.destructive_run_id
        FROM admin_groups a
        CROSS JOIN LATERAL (
            SELECT candidate.product_id
            FROM product_entitlements candidate
            JOIN products p ON p.merchant_id = candidate.merchant_id AND p.id = candidate.product_id
            WHERE candidate.merchant_id = %2$L AND candidate.entitlement = a.keys[1] AND candidate.removed_at IS NULL
              AND (SELECT array_agg(pe.entitlement ORDER BY pe.entitlement) FROM product_entitlements pe
                   WHERE pe.merchant_id = candidate.merchant_id AND pe.product_id = candidate.product_id AND pe.removed_at IS NULL) = a.keys
            ORDER BY p.archived, p.key
            LIMIT 1
        ) match
    ), ownership_windows AS (
        SELECT g.customer_id, g.product_id, CASE g.source_type WHEN 'admin' THEN 'grant' ELSE g.source_type END AS source_type,
               COALESCE(g.source_id, g.id::text) AS source_id, g.payment_id, g.starts_at, g.ends_at,
               t.id IS NOT NULL AS revoked, t.starts_at AS revoked_at, COALESCE(t.reason, t.event) AS revoke_reason, NULL::text AS destructive_run_id
        FROM grants g
        LEFT JOIN grants t ON t.merchant_id = g.merchant_id AND t.supersedes_id = g.id AND t.event IN ('revoke', 'expire', 'supersede')
        WHERE g.merchant_id = %2$L AND g.kind = 'ownership' AND g.event = 'grant' AND g.product_id IS NOT NULL
          AND NOT EXISTS (SELECT 1 FROM purchases p WHERE p.payment_id = g.payment_id AND p.product_id = g.product_id)
    )
    SELECT uuidv7() AS grant_id, uuidv7() AS access_id, c.customer_id, c.product_id, c.source_type, c.source_id, c.payment_id,
           c.starts_at, c.ends_at, c.revoked AND c.revoked_at IS NOT NULL AS revoked, c.revoked_at,
           COALESCE(c.revoke_reason, 'revoked') AS revoke_reason, c.destructive_run_id::uuid AS destructive_run_id
    FROM (
        SELECT * FROM purchases UNION ALL SELECT * FROM subscription_windows
        UNION ALL SELECT * FROM admin_windows UNION ALL SELECT * FROM ownership_windows
    ) c
    $q$, p_source, p_merchant);

    INSERT INTO billing.grants (id, merchant_id, customer_id, product_id, kind, source_type, source_id, payment_id, event, starts_at, ends_at, reason, actor, grant_reason)
    SELECT c.grant_id, p_merchant, c.customer_id, c.product_id, 'access', c.source_type, c.source_id, c.payment_id, 'grant', c.starts_at, c.ends_at,
           'product access migration', CASE WHEN c.source_type = 'grant' THEN 'migration' END, CASE WHEN c.source_type = 'grant' THEN 'migration' END
    FROM access_conversion c;
    INSERT INTO billing.grants (merchant_id, customer_id, product_id, kind, source_type, source_id, payment_id, event, supersedes_id, starts_at, reason)
    SELECT p_merchant, c.customer_id, c.product_id, 'access', c.source_type, c.source_id, c.payment_id, 'revoke', c.grant_id, c.revoked_at, c.revoke_reason
    FROM access_conversion c WHERE c.revoked;
    INSERT INTO billing.product_access (id, merchant_id, customer_id, product_id, grant_id, source_type, source_id, payment_id,
                                        starts_at, ends_at, revoked_at, revoke_reason, destructive_run_id)
    SELECT c.access_id, p_merchant, c.customer_id, c.product_id, c.grant_id, c.source_type, c.source_id, c.payment_id, c.starts_at, c.ends_at,
           CASE WHEN c.revoked THEN c.revoked_at END, CASE WHEN c.revoked THEN c.revoke_reason END, c.destructive_run_id
    FROM access_conversion c;
    GET DIAGNOSTICS converted = ROW_COUNT;

    INSERT INTO billing.grants (merchant_id, customer_id, product_id, kind, source_type, source_id, payment_id, event, supersedes_id, spec_snapshot, starts_at, reason)
    SELECT g.merchant_id, g.customer_id, g.product_id, g.kind, g.source_type, g.source_id, g.payment_id, 'supersede', g.id, g.spec_snapshot, p_at, 'product access migration'
    FROM billing.grants g
    WHERE g.merchant_id = p_merchant AND g.kind IN ('entitlement', 'ownership') AND g.event = 'grant'
      AND NOT EXISTS (SELECT 1 FROM billing.grants t WHERE t.merchant_id = g.merchant_id AND t.supersedes_id = g.id
                        AND t.event IN ('revoke', 'expire', 'supersede'));
    RETURN converted;
END $fn$;

-- Include the new financial identity tables in restore emptiness checks.
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
                  'custodians',
                  'custody_migrations',
                  'customer_delinquency',
                  'customer_entitlement_cache',
                  'customer_entitlement_cache_stamps',
                  'customer_invoice_profiles',
                  'customer_low_balance_lines',
                  'customer_links',
                  'customers',
                  'dashboard_configs',
                  'destructive_actions',
                  'destructive_holds',
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
                  'meter_period_quantities',
                  'money_settings',
                  'nmi_bulk_checkpoints',
                  'nmi_history_months',
                  'nmi_history_reads',
                  'notification_event_acks',
                  'notification_events',
                  'notifications',
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
                  'provider_bills',
                  'provider_cycle_receipts',
                  'provider_intents',
                  'provider_mutation_logs',
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
