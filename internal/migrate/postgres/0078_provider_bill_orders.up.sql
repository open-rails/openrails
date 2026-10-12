-- Repair: none-needed Prelaunch: new nullable bill fields do not reconstruct historical obligations or remove any rows.
ALTER TABLE billing.orders
    ADD COLUMN collection_owner text NOT NULL DEFAULT 'engine',
    ADD COLUMN subscription_bill_kind text;
ALTER TABLE billing.orders
    ADD CONSTRAINT orders_collection_owner_check CHECK (collection_owner IN ('engine', 'nmi_schedule', 'provider')),
    ADD CONSTRAINT orders_subscription_bill_kind_check CHECK (subscription_bill_kind IN ('initial', 'renewal', 'adjustment') AND (subscription_bill_kind IS NULL OR origin = 'subscription'));
COMMENT ON COLUMN billing.orders.collection_owner IS 'Frozen collection authority: engine, NMI schedule with existing OpenRails dunning, or provider billing. A provider bill is never a generic local charge command.';
COMMENT ON COLUMN billing.orders.subscription_bill_kind IS 'Initial observed enrollment, renewal, or a separate adjustment bill. Provider bills use scoped provider identity rather than period timestamps as document identity.';
DROP INDEX billing.orders_renewal_key;
CREATE UNIQUE INDEX orders_renewal_key ON billing.orders (merchant_id, subscription_id, period_starts_at)
    WHERE origin = 'subscription' AND collection_owner = 'engine' AND subscription_bill_kind = 'renewal';

-- Observing two genuine captures must retain both. Execution remains fenced
-- by locked orders and durable provider intents, not by discarding receipts.
DROP INDEX billing.payments_order_id_charge_key;

ALTER TABLE billing.order_lines ADD COLUMN provider_amount boolean NOT NULL DEFAULT false;
ALTER TABLE billing.order_lines DROP CONSTRAINT order_lines_amount_check;
ALTER TABLE billing.order_lines ADD CONSTRAINT order_lines_amount_check CHECK (
    (NOT provider_amount AND unit_amount >= 0 AND amount = unit_amount * COALESCE(quantity, 1))
    OR (provider_amount AND (amount >= 0 OR source = 'provider_adjustment')));
COMMENT ON COLUMN billing.order_lines.provider_amount IS 'The finalized provider amount is authoritative, including exact seat rounding and explicit signed provider adjustments; quantity and unit amount remain the accepted snapshot.';

CREATE TABLE billing.provider_bills (
    merchant_id uuid NOT NULL,
    psp_id uuid NOT NULL,
    source_kind text NOT NULL,
    source_key text NOT NULL,
    order_id uuid NOT NULL,
    customer_id uuid NOT NULL,
    snapshot jsonb NOT NULL,
    provider_reference text NOT NULL,
    settlement_source text,
    non_cash_amount bigint NOT NULL DEFAULT 0,
    settled_at timestamp with time zone,
    created_at timestamp with time zone NOT NULL DEFAULT now(),
    CONSTRAINT provider_bills_pkey PRIMARY KEY (merchant_id, psp_id, source_kind, source_key),
    CONSTRAINT provider_bills_psp_id_fkey FOREIGN KEY (merchant_id, psp_id) REFERENCES billing.psps(merchant_id, id) ON DELETE RESTRICT,
    CONSTRAINT provider_bills_order_id_fkey FOREIGN KEY (merchant_id, customer_id, order_id) REFERENCES billing.orders(merchant_id, customer_id, id) ON DELETE RESTRICT,
    CONSTRAINT provider_bills_identity_check CHECK (octet_length(source_kind) BETWEEN 1 AND 128 AND octet_length(source_key) BETWEEN 1 AND 512 AND octet_length(provider_reference) <= 512),
    CONSTRAINT provider_bills_snapshot_check CHECK (jsonb_typeof(snapshot) = 'object' AND octet_length(snapshot::text) <= 262144),
    CONSTRAINT provider_bills_settlement_check CHECK (non_cash_amount >= 0 AND (settled_at IS NULL) = (settlement_source IS NULL) AND settlement_source IN ('cash', 'credit', 'out_of_band', 'mixed'))
);
CREATE INDEX provider_bills_order_id_idx ON billing.provider_bills (merchant_id, order_id);
COMMENT ON TABLE billing.provider_bills IS 'Scoped immutable provider bill identity and accepted order snapshot. Multiple verified aliases can name one order. Settlement proof never executes collection. Retention: permanent with the financial order.';
ALTER TABLE billing.orders DROP CONSTRAINT orders_complete_payment_check;
ALTER TABLE billing.orders ADD CONSTRAINT orders_complete_payment_check CHECK (
    status <> 'complete' OR total = 0 OR payment_id IS NOT NULL OR origin = 'usage' OR collection_owner <> 'engine');

-- Ending access cannot cancel or forgive an independently issued provider bill.
CREATE OR REPLACE FUNCTION billing.void_ended_renewal_orders() RETURNS trigger
    LANGUAGE plpgsql SET search_path TO 'pg_catalog', 'billing', 'pg_temp' AS $$
BEGIN
    IF NEW.status = 'canceled' OR NEW.deleted_at IS NOT NULL THEN
        WITH voided AS (
            UPDATE billing.orders o
            SET status = 'void', payment_status = 'requires_payment_method', voided_at = now(), updated_at = now()
            WHERE o.merchant_id = NEW.merchant_id AND o.subscription_id = NEW.id AND o.origin = 'subscription' AND o.collection_owner = 'engine' AND o.status = 'open'
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

-- A provider owns the bill's lifetime. A newly observed one-off bill has no
-- invented local expiry; existing checkout expiry remains historical only.
ALTER TABLE billing.orders DROP CONSTRAINT orders_period_check;
ALTER TABLE billing.orders ADD CONSTRAINT orders_period_check CHECK (
    (origin IN ('subscription', 'usage')) = (period_starts_at IS NOT NULL AND period_ends_at IS NOT NULL AND due_at IS NOT NULL)
    AND (origin = 'subscription') = (subscription_id IS NOT NULL)
    AND (collection_owner <> 'engine' OR (origin IN ('subscription', 'usage')) = (expires_at IS NULL))
    AND (period_ends_at IS NULL OR period_ends_at > period_starts_at));
