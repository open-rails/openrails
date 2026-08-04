SET LOCAL statement_timeout = '60s';
SET LOCAL lock_timeout = '10s';

CREATE OR REPLACE FUNCTION openrails.enqueue_payment_settlement_event()
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
        INSERT INTO openrails.payment_settlement_events (merchant_id, payment_id, amount, currency, settled_at)
        VALUES (NEW.merchant_id, NEW.id, NEW.amount, NEW.currency, COALESCE(NEW.purchased_at, NEW.created_at, now()))
        ON CONFLICT (payment_id) DO NOTHING;
    END IF;
    RETURN NEW;
END;
$$;

ALTER TABLE openrails.payments DROP CONSTRAINT IF EXISTS chk_payments_money_movement;
ALTER TABLE openrails.payments DROP COLUMN IF EXISTS money_movement;
