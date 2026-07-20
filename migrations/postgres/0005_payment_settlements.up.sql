CREATE TABLE openrails.payment_settlement_events (
    id uuid DEFAULT uuidv7() PRIMARY KEY,
    merchant_id uuid NOT NULL REFERENCES openrails.merchants(id) ON DELETE CASCADE,
    payment_id uuid NOT NULL REFERENCES openrails.payments(id) ON DELETE CASCADE,
    settled_at timestamp with time zone NOT NULL DEFAULT now(),
    delivered_at timestamp with time zone,
    CONSTRAINT uq_payment_settlement_events_payment UNIQUE (payment_id)
);

CREATE INDEX idx_payment_settlement_events_pending
    ON openrails.payment_settlement_events (id)
    WHERE delivered_at IS NULL;

COMMENT ON TABLE openrails.payment_settlement_events IS
    'Durable host-consumption queue for real successful payments; consumers ack after idempotent processing.';

GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE openrails.payment_settlement_events TO openrails_app;

CREATE FUNCTION openrails.enqueue_payment_settlement_event()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF NEW.status = 'completed'
       AND NEW.amount > 0
       AND NEW.refunded_payment_id IS NULL
       AND NEW.transaction_id NOT LIKE 'sub:%'
       AND NEW.transaction_id NOT LIKE 'nmi_sub_attempt:%'
       AND NEW.transaction_id NOT LIKE 'nmi_sale_attempt:%'
       AND (TG_OP = 'INSERT' OR OLD.status IS DISTINCT FROM NEW.status)
    THEN
        INSERT INTO openrails.payment_settlement_events (merchant_id, payment_id, settled_at)
        VALUES (NEW.merchant_id, NEW.id, COALESCE(NEW.purchased_at, NEW.created_at, now()))
        ON CONFLICT (payment_id) DO NOTHING;
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER payments_enqueue_settlement_event
AFTER INSERT OR UPDATE OF status ON openrails.payments
FOR EACH ROW
EXECUTE FUNCTION openrails.enqueue_payment_settlement_event();
