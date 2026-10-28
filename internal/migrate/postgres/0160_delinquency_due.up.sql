-- Repair: none-needed A new nullable column, armed below for every payer with
-- an open usage order or a state that is not current.
-- A payer's delinquency is evaluated in the transaction that changes one of its
-- usage orders, and at next_evaluation_at when time alone changes it: an order
-- falling due or a grace window running out. A payer with an open usage order
-- has a row, current until one falls due.
ALTER TABLE billing.customer_delinquency ADD COLUMN next_evaluation_at timestamptz;
CREATE INDEX customer_delinquency_due_idx ON billing.customer_delinquency (next_evaluation_at, merchant_id, customer_id, currency)
    WHERE next_evaluation_at IS NOT NULL;
COMMENT ON COLUMN billing.customer_delinquency.next_evaluation_at IS 'The next instant time alone changes this payer''s delinquency in the currency: a usage order falling due or its grace window running out. Null: only a change to its usage orders or policy does.';
COMMENT ON TABLE billing.customer_delinquency IS 'Per-(merchant, payer, currency) arrears delinquency state: current -> grace -> delinquent, derived from overdue open usage orders against the merchant''s declared grace window and amount floor. Evaluated when a usage order of the payer changes and at next_evaluation_at; a payer with an open usage order has a row. A projection of order truth; only the transition watermarks (entered_at, transition_seq) are not recomputable. Delinquency NEVER revokes an entitlement: it refuses new spend at admission and emits a host_outbox signal; the operator owns the shutoff.';

INSERT INTO billing.customer_delinquency (merchant_id, customer_id, currency, state, overdue_amount, overdue_orders,
    entered_at, transition_seq, evaluated_at, next_evaluation_at)
SELECT DISTINCT o.merchant_id, o.customer_id, o.currency, 'current', 0, 0, now(), 0, now(), now()
FROM billing.orders o
WHERE o.origin = 'usage' AND o.status IN ('open', 'processing') AND o.amount_paid < o.total
ON CONFLICT (merchant_id, customer_id, currency) DO UPDATE SET next_evaluation_at = now();
UPDATE billing.customer_delinquency SET next_evaluation_at = now() WHERE state <> 'current';
