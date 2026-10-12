-- Repair: none-needed Renames in place: every stored row keeps its values, and the two vocabularies that change are rewritten here.
-- The word "checkout" leaves the schema: an order has a pay URL on the
-- payment origin (/pay/<order>#<secret>), and its per-PSP attempts are order
-- attempts.
ALTER TABLE billing.checkout_attempts RENAME TO order_attempts;
ALTER TABLE billing.order_attempts RENAME CONSTRAINT checkout_attempts_pkey TO order_attempts_pkey;
ALTER TABLE billing.order_attempts RENAME CONSTRAINT checkout_attempts_currency_check TO order_attempts_currency_check;
ALTER TABLE billing.order_attempts RENAME CONSTRAINT checkout_attempts_customer_id_fkey TO order_attempts_customer_id_fkey;
ALTER TABLE billing.order_attempts RENAME CONSTRAINT checkout_attempts_customer_id_payment_id_fkey TO order_attempts_customer_id_payment_id_fkey;
ALTER TABLE billing.order_attempts RENAME CONSTRAINT checkout_attempts_customer_id_subscription_id_fkey TO order_attempts_customer_id_subscription_id_fkey;
ALTER TABLE billing.order_attempts RENAME CONSTRAINT checkout_attempts_destructive_run_id_destructive_run_class_fkey TO order_attempts_destructive_run_id_destructive_run_class_fkey;
ALTER TABLE billing.order_attempts RENAME CONSTRAINT checkout_attempts_merchant_id_fkey TO order_attempts_merchant_id_fkey;
ALTER TABLE billing.order_attempts RENAME CONSTRAINT checkout_attempts_order_id_fkey TO order_attempts_order_id_fkey;
ALTER TABLE billing.order_attempts RENAME CONSTRAINT checkout_attempts_order_terms_check TO order_attempts_order_terms_check;
ALTER TABLE billing.order_attempts RENAME CONSTRAINT checkout_attempts_psp_id_rail_fkey TO order_attempts_psp_id_rail_fkey;
ALTER TABLE billing.order_attempts RENAME CONSTRAINT checkout_attempts_status_check TO order_attempts_status_check;
ALTER TABLE billing.order_attempts RENAME CONSTRAINT checkout_attempts_id_not_null TO order_attempts_id_not_null;
ALTER TABLE billing.order_attempts RENAME CONSTRAINT checkout_attempts_rail_not_null TO order_attempts_rail_not_null;
ALTER TABLE billing.order_attempts RENAME CONSTRAINT checkout_attempts_status_not_null TO order_attempts_status_not_null;
ALTER TABLE billing.order_attempts RENAME CONSTRAINT checkout_attempts_created_at_not_null TO order_attempts_created_at_not_null;
ALTER TABLE billing.order_attempts RENAME CONSTRAINT checkout_attempts_updated_at_not_null TO order_attempts_updated_at_not_null;
ALTER TABLE billing.order_attempts RENAME CONSTRAINT checkout_attempts_merchant_id_not_null TO order_attempts_merchant_id_not_null;
ALTER TABLE billing.order_attempts RENAME CONSTRAINT checkout_attempts_customer_id_not_null TO order_attempts_customer_id_not_null;
ALTER TABLE billing.order_attempts RENAME CONSTRAINT checkout_attempts_psp_id_not_null TO order_attempts_psp_id_not_null;
ALTER INDEX billing.checkout_attempts_customer_id_idx RENAME TO order_attempts_customer_id_idx;
ALTER INDEX billing.checkout_attempts_destructive_run_id_idx RENAME TO order_attempts_destructive_run_id_idx;
ALTER INDEX billing.checkout_attempts_expires_at_abandoned_idx RENAME TO order_attempts_expires_at_abandoned_idx;
ALTER INDEX billing.checkout_attempts_expires_at_expirable_idx RENAME TO order_attempts_expires_at_expirable_idx;
ALTER INDEX billing.checkout_attempts_order_id_idx RENAME TO order_attempts_order_id_idx;
ALTER INDEX billing.checkout_attempts_order_id_live_key RENAME TO order_attempts_order_id_live_key;
ALTER INDEX billing.checkout_attempts_payment_id_idx RENAME TO order_attempts_payment_id_idx;
ALTER INDEX billing.checkout_attempts_psp_id_idx RENAME TO order_attempts_psp_id_idx;
ALTER INDEX billing.checkout_attempts_psp_id_reference_key RENAME TO order_attempts_psp_id_reference_key;
ALTER INDEX billing.checkout_attempts_psp_id_transaction_id_key RENAME TO order_attempts_psp_id_transaction_id_key;
ALTER INDEX billing.checkout_attempts_subscription_id_idx RENAME TO order_attempts_subscription_id_idx;
ALTER INDEX billing.checkout_attempts_transaction_id_key RENAME TO order_attempts_transaction_id_key;

ALTER TABLE billing.solana_pay_references RENAME COLUMN checkout_attempt_id TO order_attempt_id;
ALTER TABLE billing.solana_pay_references RENAME CONSTRAINT solana_pay_references_checkout_attempt_id_fkey TO solana_pay_references_order_attempt_id_fkey;
ALTER TABLE billing.solana_pay_references RENAME CONSTRAINT solana_pay_references_checkout_attempt_id_key TO solana_pay_references_order_attempt_id_key;
ALTER TABLE billing.solana_pay_references RENAME CONSTRAINT solana_pay_references_checkout_attempt_id_not_null TO solana_pay_references_order_attempt_id_not_null;
ALTER TABLE billing.solana_pay_receipts RENAME COLUMN checkout_attempt_id TO order_attempt_id;
ALTER TABLE billing.solana_pay_receipts RENAME CONSTRAINT solana_pay_receipts_checkout_attempt_id_fkey TO solana_pay_receipts_order_attempt_id_fkey;
ALTER TABLE billing.solana_pay_receipts RENAME CONSTRAINT solana_pay_receipts_checkout_attempt_id_not_null TO solana_pay_receipts_order_attempt_id_not_null;
ALTER INDEX billing.solana_pay_receipts_checkout_attempt_id_idx RENAME TO solana_pay_receipts_order_attempt_id_idx;
COMMENT ON TABLE billing.solana_pay_receipts IS 'Every signature observed on a Solana Pay reference, recorded once. credited = the order attempt was paid by it (overpaid flags the excess for refund); review = money that was not credited (already_paid, late, underpaid, session_closed, wrong_asset, unreadable, settle_failed) and needs a refund or operator decision, closed by resolved_at; duplicate = the transfer already settled another reference; ignored = no value to the merchant (deleted with its reference). A transfer to one recipient in one mint is credited or reviewed at most once across every reference. Unresolved reviews refuse the billing archive. Retention: permanent for credited and review receipts; an ignored receipt goes with its settled reference.';
COMMENT ON TABLE billing.solana_pay_references IS 'One Solana Pay reference per order attempt. pending = awaiting a transfer landed by expires_at; confirmed = one signature credited (or mirrored); expired = nothing credited by expires_at. Purchase references stay watched until watch_ends_at so a second or late transfer is recorded, then retention deletes the settled row. seen_until is the newest signature whose older history is fully processed; scan_stack holds the before-cursors of an unfinished walk down the history and scan_below the cursor whose older signatures were just processed, so no signature is ever skipped however many land on the reference; a reference is never collected mid-walk. built_transaction is the one transaction-request tx offered while its blockhash can still land. Retention: settled references are deleted after their 7-day watch window.';

-- Provider intents name their order attempt.
DROP INDEX billing.provider_intents_checkout_attempt_idx;
UPDATE billing.provider_intents
SET payload = (payload - 'checkout_attempt_id') || jsonb_build_object('order_attempt_id', payload -> 'checkout_attempt_id')
WHERE payload ? 'checkout_attempt_id';
UPDATE billing.provider_intents
SET payload = (payload - 'checkout_idempotency_key') || jsonb_build_object('purchase_idempotency_key', payload -> 'checkout_idempotency_key')
WHERE payload ? 'checkout_idempotency_key';
CREATE INDEX provider_intents_order_attempt_idx ON billing.provider_intents
    USING btree (merchant_id, ((payload ->> 'order_attempt_id'::text))) WHERE (payload ? 'order_attempt_id'::text);

-- The order's pay URL.
ALTER TABLE billing.orders DROP CONSTRAINT orders_checkout_check;
ALTER TABLE billing.orders DROP CONSTRAINT orders_checkout_ui_mode_check;
DROP INDEX billing.orders_checkout_embedded_idx;
ALTER TABLE billing.orders RENAME COLUMN checkout_secret_hash TO pay_url_secret_hash;
ALTER TABLE billing.orders RENAME COLUMN checkout_expires_at TO pay_url_expires_at;
ALTER TABLE billing.orders RENAME COLUMN checkout_success_url TO success_url;
ALTER TABLE billing.orders RENAME COLUMN checkout_cancel_url TO cancel_url;
ALTER TABLE billing.orders RENAME COLUMN checkout_saved_payment_methods TO saved_payment_methods;
ALTER TABLE billing.orders ADD COLUMN embedded boolean;
UPDATE billing.orders SET embedded = (checkout_ui_mode = 'embedded') WHERE checkout_ui_mode IS NOT NULL;
ALTER TABLE billing.orders DROP COLUMN checkout_ui_mode;
ALTER TABLE billing.orders RENAME CONSTRAINT orders_checkout_success_url_check TO orders_success_url_check;
ALTER TABLE billing.orders RENAME CONSTRAINT orders_checkout_cancel_url_check TO orders_cancel_url_check;
ALTER INDEX billing.orders_checkout_secret_hash_key RENAME TO orders_pay_url_secret_hash_key;
ALTER TABLE billing.orders
    ADD CONSTRAINT orders_pay_url_check CHECK (
        (pay_url_secret_hash IS NULL) = (pay_url_expires_at IS NULL)
        AND (pay_url_secret_hash IS NULL) = (saved_payment_methods IS NULL)
        AND (pay_url_secret_hash IS NULL) = (embedded IS NULL)
        AND (success_url IS NULL OR pay_url_secret_hash IS NOT NULL)
        AND (cancel_url IS NULL OR pay_url_secret_hash IS NOT NULL)
        AND (pay_url_secret_hash IS NULL OR (octet_length(pay_url_secret_hash) = 32 AND pay_url_expires_at <= expires_at
            AND ((origin = 'merchant' AND success_url IS NOT NULL)
                OR (origin = 'customer' AND success_url IS NULL AND cancel_url IS NULL AND embedded))))
    );
CREATE INDEX orders_embedded_idx ON billing.orders USING btree (id) WHERE embedded;
COMMENT ON COLUMN billing.orders.attempt_id IS 'The order attempt last started for the order.';
COMMENT ON COLUMN billing.orders.pay_url_secret_hash IS 'sha256 of the pay URL''s secret; the secret is never stored. Set once on a merchant order; on a customer''s own order, again each time the customer asks for its pay URL, which ends the last.';
COMMENT ON COLUMN billing.orders.pay_url_expires_at IS 'When the pay URL stops working; no later than the order''s own expiry.';
COMMENT ON COLUMN billing.orders.embedded IS 'The merchant''s page frames the pay URL, only on its registered origins (settings payment_page.embed_origins); otherwise the customer is sent to it.';
COMMENT ON COLUMN billing.orders.success_url IS 'Where a merchant order''s pay URL returns once paid. NULL on a customer''s own embedded pay URL, which returns nowhere: the frame tells its page.';
COMMENT ON TABLE billing.orders IS 'One payable (ord_ id): frozen lines and total in one currency, paid by at most one live order attempt at a time: a purchase, or a renewal of an engine-owned subscription. A decline leaves it open with last_payment_error; it is numbered from document_sequences when complete. idempotency_key is the scoped Idempotency-Key of the request that created it, kept as long as the order. Retention: unpaid canceled and expired orders that never started a payment attempt are deleted 90 days after they closed; every other order is permanent.';
UPDATE billing.idempotency_keys SET operation = 'pay_url_creation' WHERE operation = 'hosted_checkout_creation';

-- A purchase groups one buyer's payment attempts on one target.
ALTER TABLE billing.payment_attempts RENAME COLUMN checkout_id TO purchase_id;
ALTER TABLE billing.payment_attempts RENAME COLUMN checkout_target TO purchase_target;
ALTER TABLE billing.payment_attempts RENAME CONSTRAINT payment_attempts_checkout_check TO payment_attempts_purchase_check;
ALTER INDEX billing.payment_attempts_checkout_id_attempted_at_idx RENAME TO payment_attempts_purchase_id_attempted_at_idx;
ALTER INDEX billing.payment_attempts_customer_id_checkout_target_attempted_at_idx RENAME TO payment_attempts_customer_id_purchase_target_attempted_at_idx;
COMMENT ON TABLE billing.payment_attempts IS 'One row per authorization answered by a PSP: the $0 card verification, sales, rebills and retries. Never the PAN or CVV. purchase_id groups one buyer''s attempts on one target (purchase_target: a price id or card_save) until the target is approved. Retention: rows are deleted 25 months (761 days) after attempted_at.';

-- A link proven on the payment page.
ALTER TABLE billing.customer_links DROP CONSTRAINT customer_links_method_check;
ALTER TABLE billing.customer_links DROP CONSTRAINT customer_links_proof_check;
UPDATE billing.customer_links SET method = 'payment_page', credential = 'pay_url' WHERE method = 'checkout';
ALTER TABLE billing.customer_links
    ADD CONSTRAINT customer_links_method_check CHECK (method IN ('payment_page', 'connect', 'sign_in')),
    ADD CONSTRAINT customer_links_proof_check CHECK (
        (method = 'payment_page' AND credential = 'pay_url' AND order_id IS NOT NULL AND auth_time IS NOT NULL AND amr IS NOT NULL)
        OR (method = 'connect' AND credential = 'customer_token' AND order_id IS NULL AND auth_time IS NOT NULL AND amr IS NOT NULL)
        OR (method = 'sign_in' AND credential IS NULL AND order_id IS NULL AND auth_time IS NULL AND amr IS NULL AND customer_id = account_id)
    );
COMMENT ON COLUMN billing.customer_links.method IS 'payment_page: on the payment page, by an order''s pay URL; connect: the merchant''s own customer, from its site; sign_in: the merchant signs its customers in with openrails.dev, so the customer is the account itself.';
COMMENT ON COLUMN billing.customer_links.credential IS 'The merchant-side proof: the customer''s token at the merchant, or the pay URL secret of order_id. NULL for sign_in.';
COMMENT ON COLUMN billing.customer_links.order_id IS 'The order whose pay URL made the link; evidence only, kept after the order goes.';

COMMENT ON COLUMN billing.payments.attempt_kind IS 'initial|renewal, stamped at write time by the purchase vs rebill paths; NULL = unknown (imported/pre-instrumentation rows).';
COMMENT ON COLUMN billing.products.credit_grant IS 'Purchased currency credit policy; accepted purchases freeze amount and expiry duration.';

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
                  'order_attempts',
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
                  'provider_charge_attributions',
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
