-- or#827: the host settlement feed decided what is real money by what a
-- transaction_id did NOT look like.
--
-- 0005's enqueue trigger filtered `transaction_id NOT LIKE 'sub:%' /
-- 'nmi_sub_attempt:%' / 'nmi_sale_attempt:%'`. A denylist is open at the top:
-- every synthetic shape invented afterwards defaults to "real money" and
-- publishes a host-visible settlement event for a payment that never moved a
-- cent. Three such shapes already exist — #796's 'nmi_sale_declined:',
-- 'nmi_sub_declined:' and 'vaulted_card_sale_declined:' — and are outside the
-- denylist; only their status='failed' keeps them out of the feed today.
--
-- money_movement states the fact positively:
--
--   'rail' — this row records money that actually moved at the payment rail;
--            transaction_id is the rail's own reference.
--   'none' — a bookkeeping row: an attempt anchor whose real charge is
--            recorded on a different row, a decline, a placeholder.
--
-- The DEFAULT is the fail-closed value. A writer that declares nothing
-- publishes nothing; a new synthetic shape can no longer leak into the feed by
-- omission. The reverse mistake (a real charge that forgets to say 'rail')
-- withholds one event instead of inventing one, and is caught loudly in Go:
-- payments.paymentInsertParams rejects an undeclared completed positive charge.

BEGIN;

ALTER TABLE openrails.payments
    ADD COLUMN money_movement text NOT NULL DEFAULT 'none';

ALTER TABLE openrails.payments
    ADD CONSTRAINT chk_payments_money_movement
    CHECK (money_movement = ANY (ARRAY['rail'::text, 'none'::text]));

COMMENT ON COLUMN openrails.payments.money_movement IS
    'or#827 rail|none — positive marker for real money movement at the payment rail. ''rail'' rows carry a rail-issued transaction_id and are the ONLY rows the host settlement feed publishes; ''none'' rows are bookkeeping (attempt anchors, declines, placeholders). Fail-closed default: undeclared = ''none''.';

-- One-time historical classification. Applying the retired denylist ONCE, at
-- the boundary, is how rows that predate the column get an honest value; it is
-- not a runtime gate. Only a settled or later-reversed charge moved money —
-- 'pending' has not moved yet and 'failed' never did.
UPDATE openrails.payments
   SET money_movement = 'rail'
 WHERE status IN ('completed', 'refunded')
   AND transaction_id NOT LIKE 'sub:%'
   AND transaction_id NOT LIKE 'nmi_sub_attempt:%'
   AND transaction_id NOT LIKE 'nmi_sale_attempt:%'
   AND transaction_id NOT LIKE 'nmi_sale_declined:%'
   AND transaction_id NOT LIKE 'nmi_sub_declined:%'
   AND transaction_id NOT LIKE 'vaulted_card_sale_declined:%';

CREATE OR REPLACE FUNCTION openrails.enqueue_payment_settlement_event()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF NEW.status = 'completed'
       AND NEW.amount > 0
       AND NEW.refunded_payment_id IS NULL
       AND NEW.money_movement = 'rail'
       AND (TG_OP = 'INSERT' OR OLD.status IS DISTINCT FROM NEW.status)
    THEN
        INSERT INTO openrails.payment_settlement_events (merchant_id, payment_id, amount, currency, settled_at)
        VALUES (NEW.merchant_id, NEW.id, NEW.amount, NEW.currency, COALESCE(NEW.purchased_at, NEW.created_at, now()))
        ON CONFLICT (payment_id) DO NOTHING;
    END IF;
    RETURN NEW;
END;
$$;

COMMIT;
