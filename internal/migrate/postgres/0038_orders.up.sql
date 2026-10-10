-- parent: 37 sha256:05bfe0cd6102160d3456cf13d6898b9ba9822289b699bd2632c34b50c235952b
-- Orders (#1168): a customer's purchase of priced lines with frozen totals,
-- paid through checkout attempts; ownership claims guard concurrent buys;
-- one gapless document sequence per merchant numbers what is paid.

-- A product's ownership rule; NULL derives it from the product and its prices.
ALTER TABLE billing.products ADD COLUMN ownership text
    CONSTRAINT products_ownership_check CHECK (ownership IN ('unique', 'consumable', 'extend'));
COMMENT ON COLUMN billing.products.ownership IS 'unique (one live holding per customer, per tier group when set), consumable (units stack, quantity counts them) or extend (a later purchase starts when the current window ends). NULL derives it: consumable for a credit product, extend when every price is one-time with an access duration, unique otherwise.';

CREATE TABLE billing.document_sequences (
    merchant_id uuid NOT NULL,
    last_number bigint NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT document_sequences_pkey PRIMARY KEY (merchant_id),
    CONSTRAINT document_sequences_merchant_id_fkey FOREIGN KEY (merchant_id) REFERENCES billing.merchants(id) ON DELETE RESTRICT,
    CONSTRAINT document_sequences_last_number_check CHECK (last_number >= 1)
);
COMMENT ON TABLE billing.document_sequences IS 'The merchant''s gapless document number: the last one issued. A number is taken in the transaction that pays an order, so an abandoned order consumes none.';

CREATE TABLE billing.orders (
    merchant_id uuid NOT NULL,
    id uuid DEFAULT uuidv7() NOT NULL,
    customer_id uuid NOT NULL,
    origin text NOT NULL,
    status text NOT NULL,
    currency text NOT NULL,
    total bigint NOT NULL,
    number text,
    idempotency_key text,
    request_digest bytea,
    payment_method_id uuid,
    psp_id uuid,
    attempt_id uuid,
    payment_id uuid,
    last_payment_error jsonb,
    expires_at timestamp with time zone NOT NULL,
    paid_at timestamp with time zone,
    canceled_at timestamp with time zone,
    expired_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT orders_pkey PRIMARY KEY (merchant_id, id),
    CONSTRAINT orders_customer_id_id_key UNIQUE (merchant_id, customer_id, id),
    CONSTRAINT orders_merchant_id_fkey FOREIGN KEY (merchant_id) REFERENCES billing.merchants(id) ON DELETE RESTRICT,
    CONSTRAINT orders_customer_id_fkey FOREIGN KEY (merchant_id, customer_id) REFERENCES billing.customers(merchant_id, id),
    CONSTRAINT orders_payment_method_id_fkey FOREIGN KEY (merchant_id, payment_method_id) REFERENCES billing.payment_methods(merchant_id, id),
    CONSTRAINT orders_psp_id_fkey FOREIGN KEY (merchant_id, psp_id) REFERENCES billing.psps(merchant_id, id),
    CONSTRAINT orders_payment_id_fkey FOREIGN KEY (merchant_id, customer_id, payment_id) REFERENCES billing.payments(merchant_id, customer_id, id) DEFERRABLE INITIALLY DEFERRED,
    CONSTRAINT orders_origin_check CHECK (origin IN ('customer', 'merchant')),
    CONSTRAINT orders_status_check CHECK (status IN ('open', 'requires_action', 'processing', 'paid', 'canceled', 'expired')),
    CONSTRAINT orders_currency_check CHECK (currency ~ '^[A-Z0-9]{3,12}$'),
    CONSTRAINT orders_total_check CHECK (total >= 0),
    CONSTRAINT orders_number_check CHECK (number <> ''),
    CONSTRAINT orders_idempotency_key_check CHECK (octet_length(idempotency_key) BETWEEN 1 AND 512),
    CONSTRAINT orders_request_digest_check CHECK ((idempotency_key IS NULL) = (request_digest IS NULL) AND (request_digest IS NULL OR octet_length(request_digest) = 32)),
    CONSTRAINT orders_paid_check CHECK ((status = 'paid') = (number IS NOT NULL AND paid_at IS NOT NULL)),
    CONSTRAINT orders_paid_payment_check CHECK (status <> 'paid' OR total = 0 OR payment_id IS NOT NULL),
    CONSTRAINT orders_canceled_check CHECK (status <> 'canceled' OR canceled_at IS NOT NULL),
    CONSTRAINT orders_expired_check CHECK (status <> 'expired' OR expired_at IS NOT NULL)
);
COMMENT ON TABLE billing.orders IS 'One purchase (ord_ id): frozen lines and total in one currency, paid by at most one live checkout attempt at a time. A decline leaves it open with last_payment_error; it is numbered from document_sequences when paid. idempotency_key is the scoped Idempotency-Key of the request that created it, kept as long as the order. Retention: unpaid canceled and expired orders that never started a payment attempt are deleted 90 days after they closed; every other order is permanent.';
COMMENT ON COLUMN billing.orders.payment_method_id IS 'The saved card the latest attempt charged; a retry may name it again.';
COMMENT ON COLUMN billing.orders.attempt_id IS 'The checkout attempt last started for the order.';

CREATE UNIQUE INDEX orders_idempotency_key_key ON billing.orders USING btree (merchant_id, customer_id, idempotency_key) WHERE idempotency_key IS NOT NULL;
CREATE UNIQUE INDEX orders_number_key ON billing.orders USING btree (merchant_id, number) WHERE number IS NOT NULL;
CREATE INDEX orders_customer_id_created_at_id_idx ON billing.orders USING btree (merchant_id, customer_id, created_at DESC, id DESC);
CREATE INDEX orders_created_at_id_idx ON billing.orders USING btree (merchant_id, created_at DESC, id DESC);
CREATE INDEX orders_expires_at_live_idx ON billing.orders USING btree (merchant_id, expires_at) WHERE status IN ('open', 'requires_action');
CREATE INDEX orders_closed_at_unpaid_idx ON billing.orders USING btree (merchant_id, (COALESCE(canceled_at, expired_at))) WHERE status IN ('canceled', 'expired') AND payment_id IS NULL AND attempt_id IS NULL;
CREATE INDEX orders_payment_method_id_idx ON billing.orders USING btree (merchant_id, payment_method_id) WHERE payment_method_id IS NOT NULL;

CREATE TABLE billing.order_lines (
    merchant_id uuid NOT NULL,
    id uuid DEFAULT uuidv7() NOT NULL,
    order_id uuid NOT NULL,
    customer_id uuid NOT NULL,
    "position" integer NOT NULL,
    price_id uuid NOT NULL,
    product_id uuid NOT NULL,
    description text NOT NULL,
    quantity integer,
    unit_amount bigint NOT NULL,
    amount bigint NOT NULL,
    ownership text NOT NULL,
    claim_key text,
    billing_interval_hours integer,
    access_duration_hours integer,
    credit_grant jsonb,
    subscription_id uuid,
    product_access_id uuid,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT order_lines_pkey PRIMARY KEY (merchant_id, id),
    CONSTRAINT order_lines_order_id_position_key UNIQUE (merchant_id, order_id, "position"),
    CONSTRAINT order_lines_order_id_fkey FOREIGN KEY (merchant_id, customer_id, order_id) REFERENCES billing.orders(merchant_id, customer_id, id) ON DELETE CASCADE,
    CONSTRAINT order_lines_price_id_fkey FOREIGN KEY (merchant_id, price_id) REFERENCES billing.prices(merchant_id, id),
    CONSTRAINT order_lines_product_id_fkey FOREIGN KEY (merchant_id, product_id) REFERENCES billing.products(merchant_id, id),
    CONSTRAINT order_lines_subscription_id_fkey FOREIGN KEY (merchant_id, subscription_id) REFERENCES billing.subscriptions(merchant_id, id),
    CONSTRAINT order_lines_product_access_id_fkey FOREIGN KEY (merchant_id, product_access_id) REFERENCES billing.product_access(merchant_id, id),
    CONSTRAINT order_lines_position_check CHECK ("position" >= 0 AND "position" < 100),
    CONSTRAINT order_lines_description_check CHECK (description <> ''),
    CONSTRAINT order_lines_quantity_check CHECK (quantity >= 1),
    CONSTRAINT order_lines_amount_check CHECK (unit_amount >= 0 AND amount = unit_amount * COALESCE(quantity, 1)),
    CONSTRAINT order_lines_ownership_check CHECK (ownership IN ('unique', 'consumable', 'extend')),
    CONSTRAINT order_lines_claim_key_check CHECK ((claim_key IS NOT NULL) = (ownership = 'unique')),
    CONSTRAINT order_lines_seats_check CHECK (quantity IS NULL OR quantity = 1 OR ownership = 'consumable' OR billing_interval_hours IS NOT NULL),
    CONSTRAINT order_lines_quantified_check CHECK (quantity IS NOT NULL OR billing_interval_hours IS NOT NULL),
    CONSTRAINT order_lines_billing_interval_hours_check CHECK (billing_interval_hours > 0),
    CONSTRAINT order_lines_access_duration_hours_check CHECK (access_duration_hours > 0)
);
COMMENT ON TABLE billing.order_lines IS 'One line of an order, frozen at creation: a price, its quantity (seats on a per-seat recurring price, units of a consumable, NULL on any other recurring price) and amounts, the ownership rule it was sold under, and what paying it produced (subscription_id, product_access_id). Retention: deleted with their order.';
COMMENT ON COLUMN billing.order_lines.claim_key IS 'product:<id> or tier_group:<group>: the ownership a unique line claims while its order is live.';

CREATE INDEX order_lines_subscription_id_idx ON billing.order_lines USING btree (merchant_id, subscription_id) WHERE subscription_id IS NOT NULL;
CREATE INDEX order_lines_price_id_idx ON billing.order_lines USING btree (merchant_id, price_id);

-- A live claim on one ownership key per customer: held by an unpaid order,
-- then by what paying it produced. It is deleted when its holder ends.
CREATE TABLE billing.ownership_claims (
    merchant_id uuid NOT NULL,
    customer_id uuid NOT NULL,
    claim_key text NOT NULL,
    order_id uuid NOT NULL,
    holder_type text NOT NULL,
    holder_id uuid NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT ownership_claims_pkey PRIMARY KEY (merchant_id, customer_id, claim_key),
    CONSTRAINT ownership_claims_customer_id_fkey FOREIGN KEY (merchant_id, customer_id) REFERENCES billing.customers(merchant_id, id),
    CONSTRAINT ownership_claims_order_id_fkey FOREIGN KEY (merchant_id, customer_id, order_id) REFERENCES billing.orders(merchant_id, customer_id, id) ON DELETE CASCADE,
    CONSTRAINT ownership_claims_claim_key_check CHECK (claim_key ~ '^(product|tier_group):.+$'),
    CONSTRAINT ownership_claims_holder_type_check CHECK (holder_type IN ('order', 'subscription', 'product_access')),
    CONSTRAINT ownership_claims_holder_check CHECK (holder_type <> 'order' OR holder_id = order_id)
);
COMMENT ON TABLE billing.ownership_claims IS 'At most one live ownership per customer and key (product or tier group). An order claims its unique lines when created; paying hands each claim to the subscription or product access it produced; cancel, expiry, the subscription''s end or the access''s revocation deletes it.';

CREATE INDEX ownership_claims_holder_idx ON billing.ownership_claims USING btree (merchant_id, holder_type, holder_id);
CREATE INDEX ownership_claims_order_id_idx ON billing.ownership_claims USING btree (merchant_id, customer_id, order_id);

-- A holder that ends releases its claims, whichever path ended it.
CREATE FUNCTION billing.release_ended_ownership_claims() RETURNS trigger
    LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
BEGIN
    IF TG_TABLE_NAME = 'subscriptions' THEN
        IF NEW.status = 'canceled' OR NEW.deleted_at IS NOT NULL THEN
            DELETE FROM billing.ownership_claims
            WHERE merchant_id = NEW.merchant_id AND holder_type = 'subscription' AND holder_id = NEW.id;
        END IF;
    ELSIF NEW.revoked_at IS NOT NULL OR NEW.deleted_at IS NOT NULL THEN
        DELETE FROM billing.ownership_claims
        WHERE merchant_id = NEW.merchant_id AND holder_type = 'product_access' AND holder_id = NEW.id;
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER subscriptions_release_ownership_claims AFTER UPDATE OF status, deleted_at ON billing.subscriptions
    FOR EACH ROW EXECUTE FUNCTION billing.release_ended_ownership_claims();
CREATE TRIGGER product_access_release_ownership_claims AFTER UPDATE OF revoked_at, deleted_at ON billing.product_access
    FOR EACH ROW EXECUTE FUNCTION billing.release_ended_ownership_claims();

-- Attempts pay a payable: an order, or (until sessions go) one price.
ALTER TABLE billing.checkout_attempts ADD COLUMN order_id uuid;
ALTER TABLE billing.checkout_attempts ADD CONSTRAINT checkout_attempts_order_id_fkey
    FOREIGN KEY (merchant_id, customer_id, order_id) REFERENCES billing.orders(merchant_id, customer_id, id);
ALTER TABLE billing.checkout_attempts DROP CONSTRAINT checkout_attempts_mode_check;
ALTER TABLE billing.checkout_attempts ADD CONSTRAINT checkout_attempts_mode_check
    CHECK (mode IN ('one_off', 'subscription', 'payment_method', 'order'));
ALTER TABLE billing.checkout_attempts DROP CONSTRAINT checkout_attempts_status_check;
ALTER TABLE billing.checkout_attempts ADD CONSTRAINT checkout_attempts_status_check
    CHECK (status IN ('created', 'requires_action', 'processing', 'succeeded', 'failed', 'expired', 'canceled'));
ALTER TABLE billing.checkout_attempts DROP CONSTRAINT checkout_attempts_monetary_terms_check;
ALTER TABLE billing.checkout_attempts ADD CONSTRAINT checkout_attempts_monetary_terms_check CHECK (
    (mode = 'payment_method' AND price_id IS NULL AND order_id IS NULL AND amount IS NULL AND currency IS NULL AND payment_id IS NULL AND subscription_id IS NULL)
    OR (mode = 'order' AND price_id IS NULL AND order_id IS NOT NULL AND amount IS NOT NULL AND currency IS NOT NULL AND subscription_id IS NULL)
    OR (mode IN ('one_off', 'subscription') AND price_id IS NOT NULL AND order_id IS NULL AND amount IS NOT NULL AND currency IS NOT NULL)
);
COMMENT ON COLUMN billing.checkout_attempts.order_id IS 'The order this attempt pays (mode order).';
CREATE UNIQUE INDEX checkout_attempts_order_id_live_key ON billing.checkout_attempts USING btree (merchant_id, order_id)
    WHERE order_id IS NOT NULL AND status IN ('created', 'requires_action', 'processing') AND deleted_at IS NULL;
CREATE INDEX checkout_attempts_order_id_idx ON billing.checkout_attempts USING btree (merchant_id, order_id) WHERE order_id IS NOT NULL;
ALTER TABLE billing.orders ADD CONSTRAINT orders_attempt_id_fkey FOREIGN KEY (merchant_id, attempt_id) REFERENCES billing.checkout_attempts(merchant_id, id) ON DELETE SET NULL (attempt_id) DEFERRABLE INITIALLY DEFERRED;

-- Payments reference what they paid: an order, a subscription's period, or
-- (a one-price sale before orders) a price.
ALTER TABLE billing.payments ADD COLUMN order_id uuid;
ALTER TABLE billing.payments ALTER COLUMN price_id DROP NOT NULL;
ALTER TABLE billing.payments ADD CONSTRAINT payments_payable_check CHECK (order_id IS NULL OR price_id IS NULL);
ALTER TABLE billing.payments ADD CONSTRAINT payments_order_id_fkey
    FOREIGN KEY (merchant_id, customer_id, order_id) REFERENCES billing.orders(merchant_id, customer_id, id);
COMMENT ON COLUMN billing.payments.order_id IS 'The order this charge (or its refund) paid; its lines say what it bought, and price_id is then NULL.';
CREATE INDEX payments_order_id_idx ON billing.payments USING btree (merchant_id, order_id) WHERE order_id IS NOT NULL;
-- At most one successful charge per order.
CREATE UNIQUE INDEX payments_order_id_charge_key ON billing.payments USING btree (merchant_id, order_id)
    WHERE order_id IS NOT NULL AND refunded_payment_id IS NULL AND status IN ('completed', 'refunded') AND deleted_at IS NULL;

-- Order events reach the host through the outbox.
ALTER TABLE billing.host_outbox DROP CONSTRAINT host_outbox_payload_check;
ALTER TABLE billing.host_outbox ADD CONSTRAINT host_outbox_payload_check CHECK (
    (event_type = 'payment.settled' AND subject_type = 'payment' AND payment_id IS NOT NULL
     AND subject_id = payment_id AND amount IS NOT NULL AND amount > 0 AND data = '{}'::jsonb)
    OR (event_type IN ('delinquency.grace', 'delinquency.entered', 'delinquency.cleared')
        AND subject_type = 'customer' AND payment_id IS NULL AND amount IS NULL)
    OR (event_type = 'product.entitlements_changed' AND subject_type = 'product' AND payment_id IS NULL AND amount IS NULL)
    OR (event_type IN ('order.paid', 'order.requires_action', 'order.payment_failed', 'order.canceled', 'order.expired')
        AND subject_type = 'order' AND payment_id IS NULL AND amount IS NOT NULL AND amount >= 0 AND currency IS NOT NULL)
);
COMMENT ON TABLE billing.host_outbox IS 'Typed durable host events: successful rail payment settlements, delinquency lifecycle transitions, product key changes and order transitions. Acknowledge after idempotent processing; acknowledgments are separate from notification read state. Retention: delivered events are deleted 30 days after delivered_at; an undelivered event is never deleted.';

-- Orders and their claims are merchant rows and occupy a restore destination.
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
                  'order_lines',
                  'orders',
                  'ownership_claims',
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
