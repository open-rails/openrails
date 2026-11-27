-- The indexes the facts-only reads plan on: covering, so a read of a
-- merchant's movements is an index-only range scan.
CREATE SCHEMA IF NOT EXISTS proto;
CREATE INDEX proto_mrr_movements_at_idx ON billing.mrr_movements (merchant_id, occurred_at)
    INCLUDE (currency, kind, delta, logo_delta, customer_id, voluntary, cause, id);
CREATE INDEX proto_mrr_movements_cohort_idx ON billing.mrr_movements (merchant_id, cohort_at)
    INCLUDE (currency, occurred_at, delta, logo_delta, customer_id);
CREATE INDEX proto_mrr_movements_logo_idx ON billing.mrr_movements (merchant_id, customer_id, occurred_at, id)
    INCLUDE (currency, logo_delta) WHERE logo_delta <> 0;
CREATE INDEX proto_mrr_risk_cases_opened_idx ON billing.mrr_risk_cases (merchant_id, opened_at)
    INCLUDE (currency, mrr_at_entry, closed_at);
CREATE INDEX proto_mrr_risk_cases_closed_idx ON billing.mrr_risk_cases (merchant_id, closed_at)
    INCLUDE (currency, mrr_at_entry, mrr_at_exit, outcome, entry_reason) WHERE closed_at IS NOT NULL;
CREATE INDEX proto_mrr_risk_episodes_entered_idx ON billing.mrr_risk_episodes (merchant_id, entered_at)
    INCLUDE (currency, reason, mrr_at_entry);
CREATE INDEX proto_mrr_risk_episodes_left_idx ON billing.mrr_risk_episodes (merchant_id, left_at)
    INCLUDE (currency, reason, mrr_at_entry) WHERE left_at IS NOT NULL;
-- Lines that carry their movement's instant and currency: what a cut would
-- write, so a product read needs no join.
CREATE TABLE proto.lines_wide AS
SELECT l.merchant_id, m.occurred_at, m.currency, l.product_id, l.delta, l.subscriber_delta
  FROM billing.mrr_movement_lines l
  JOIN billing.mrr_movements m ON m.merchant_id = l.merchant_id AND m.id = l.movement_id;
CREATE INDEX proto_lines_wide_at_idx ON proto.lines_wide (merchant_id, occurred_at) INCLUDE (currency, product_id, delta, subscriber_delta);
