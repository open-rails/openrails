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
-- A month-close per currency: MRR and customers with MRR at each UTC month
-- start, so a snapshot read starts from the close before its range.
CREATE TABLE proto.mrr_month_close (
    merchant_id uuid NOT NULL,
    currency text NOT NULL,
    at timestamptz NOT NULL,
    mrr bigint NOT NULL,
    customers bigint NOT NULL,
    PRIMARY KEY (merchant_id, at, currency)
);
CREATE TABLE proto.product_month_close (
    merchant_id uuid NOT NULL,
    currency text NOT NULL,
    product_id uuid NOT NULL,
    at timestamptz NOT NULL,
    mrr bigint NOT NULL,
    subscribers bigint NOT NULL,
    PRIMARY KEY (merchant_id, at, currency, product_id)
);
-- The customer-level logo change a movement would carry: +1 when the
-- customer gains MRR in a first currency, -1 when it loses its last.
CREATE TABLE proto.customer_logos (
    merchant_id uuid NOT NULL,
    occurred_at timestamptz NOT NULL,
    d integer NOT NULL
);
CREATE TABLE proto.customer_month_close (
    merchant_id uuid NOT NULL,
    at timestamptz NOT NULL,
    customers bigint NOT NULL,
    PRIMARY KEY (merchant_id, at)
);
