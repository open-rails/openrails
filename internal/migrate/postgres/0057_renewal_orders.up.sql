-- Repair: none-needed Every existing order is a customer's or a merchant's purchase; none renews a subscription.
-- A renewal of an engine-owned subscription is an order (origin subscription):
-- one per subscription and period, due when the period starts, with no expiry.
-- Its collection attempts are its checkout attempts; the subscription ending
-- voids it while unpaid.
ALTER TABLE billing.orders
    ADD COLUMN subscription_id uuid,
    ADD COLUMN period_starts_at timestamp with time zone,
    ADD COLUMN period_ends_at timestamp with time zone,
    ADD COLUMN due_at timestamp with time zone,
    ADD COLUMN voided_at timestamp with time zone,
    ALTER COLUMN expires_at DROP NOT NULL;
ALTER TABLE billing.orders DROP CONSTRAINT orders_origin_check;
ALTER TABLE billing.orders DROP CONSTRAINT orders_status_check;
ALTER TABLE billing.orders
    ADD CONSTRAINT orders_origin_check CHECK (origin IN ('customer', 'merchant', 'subscription')),
    ADD CONSTRAINT orders_status_check CHECK (status IN ('open', 'processing', 'complete', 'canceled', 'expired', 'void')),
    ADD CONSTRAINT orders_renewal_check CHECK (
        (origin = 'subscription') = (subscription_id IS NOT NULL AND period_starts_at IS NOT NULL AND period_ends_at IS NOT NULL AND due_at IS NOT NULL)
        AND (origin = 'subscription') = (expires_at IS NULL)
        AND (period_ends_at IS NULL OR period_ends_at > period_starts_at)),
    ADD CONSTRAINT orders_void_check CHECK ((status = 'void') = (voided_at IS NOT NULL)),
    ADD CONSTRAINT orders_subscription_id_fkey FOREIGN KEY (merchant_id, subscription_id) REFERENCES billing.subscriptions(merchant_id, id);
CREATE UNIQUE INDEX orders_renewal_key ON billing.orders USING btree (merchant_id, subscription_id, period_starts_at) WHERE origin = 'subscription';
CREATE INDEX orders_due_at_open_idx ON billing.orders USING btree (merchant_id, due_at) WHERE status = 'open' AND due_at IS NOT NULL;
COMMENT ON COLUMN billing.orders.status IS 'open (takes payment, or awaits the customer''s action on it), processing (the provider has its payment), complete (paid and fulfilled), canceled or expired (a purchase closed unpaid), void (a renewal its subscription''s end left unowed).';
COMMENT ON COLUMN billing.orders.origin IS 'customer (the customer''s own purchase), merchant (an unpaid order the merchant created for them) or subscription (a renewal of an engine-owned subscription: subscription_id, its period and due_at).';
COMMENT ON COLUMN billing.orders.due_at IS 'When a renewal is owed: its period''s start. An open order past it is overdue.';
COMMENT ON COLUMN billing.orders.expires_at IS 'When an unpaid purchase expires; a renewal has none.';
COMMENT ON TABLE billing.orders IS 'One payable (ord_ id): frozen lines and total in one currency, paid by at most one live checkout attempt at a time: a purchase, or a renewal of an engine-owned subscription. A decline leaves it open with last_payment_error; it is numbered from document_sequences when complete. idempotency_key is the scoped Idempotency-Key of the request that created it, kept as long as the order. Retention: unpaid canceled and expired orders that never started a payment attempt are deleted 90 days after they closed; every other order is permanent.';

ALTER TABLE billing.host_outbox DROP CONSTRAINT host_outbox_payload_check;
ALTER TABLE billing.host_outbox ADD CONSTRAINT host_outbox_payload_check CHECK (
    (event_type = 'payment.settled' AND subject_type = 'payment' AND payment_id IS NOT NULL
     AND subject_id = payment_id AND amount IS NOT NULL AND amount > 0 AND data = '{}'::jsonb)
    OR (event_type IN ('delinquency.grace', 'delinquency.entered', 'delinquency.cleared')
        AND subject_type = 'customer' AND payment_id IS NULL AND amount IS NULL)
    OR (event_type = 'product.entitlements_changed' AND subject_type = 'product' AND payment_id IS NULL AND amount IS NULL)
    OR (event_type IN ('order.completed', 'order.requires_action', 'order.payment_failed', 'order.canceled', 'order.expired', 'order.voided')
        AND subject_type = 'order' AND payment_id IS NULL AND amount IS NOT NULL AND amount >= 0 AND currency IS NOT NULL)
    OR (event_type IN ('customer.network_linked', 'customer.network_unlinked')
        AND subject_type = 'customer' AND payment_id IS NULL AND amount IS NULL)
);

-- A subscription that ends voids its unpaid renewals, whichever path ended
-- it. A renewal whose payment is with the provider waits for its answer,
-- whose settlement voids it.
CREATE FUNCTION billing.void_ended_renewal_orders() RETURNS trigger
    LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
BEGIN
    IF NEW.status = 'canceled' OR NEW.deleted_at IS NOT NULL THEN
        WITH voided AS (
            UPDATE billing.orders o
            SET status = 'void', payment_status = 'requires_payment_method', voided_at = now(), updated_at = now()
            WHERE o.merchant_id = NEW.merchant_id AND o.subscription_id = NEW.id AND o.origin = 'subscription' AND o.status = 'open'
            RETURNING o.id, o.customer_id, o.total, o.currency)
        INSERT INTO billing.host_outbox (merchant_id, event_type, subject_type, subject_id, amount, currency, occurred_at, data, dedupe_key)
        SELECT NEW.merchant_id, 'order.voided', 'order', v.id, v.total, v.currency, now(),
               jsonb_build_object('customer_id', v.customer_id, 'status', 'void', 'payment_status', 'requires_payment_method'),
               'order.voided:' || v.id
        FROM voided v
        ON CONFLICT (merchant_id, dedupe_key) DO NOTHING;
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER subscriptions_void_renewal_orders AFTER UPDATE OF status, deleted_at ON billing.subscriptions
    FOR EACH ROW EXECUTE FUNCTION billing.void_ended_renewal_orders();
