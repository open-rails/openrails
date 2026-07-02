-- #672: metered-usage rated-through watermark. Invoice closes rate usage over
-- [period_from, cutoff); threshold closes anchor `from` at the period start, so
-- successive closes over one period rate OVERLAPPING prefixes. One row per
-- (payer, currency, meter source, period start) records how much has already
-- been accrued for the period prefix; the sweep accrues only
-- rate-the-full-prefix-once minus accrued_amount, atomically with the ledger
-- transfer. Invariant: each unit of usage is rated exactly once per period;
-- rated_through and accrued_amount are monotone.
CREATE TABLE openrails.metered_rating_watermarks (
    merchant_id uuid NOT NULL,
    customer_id uuid NOT NULL,
    currency text NOT NULL,
    source text NOT NULL,
    period_from timestamp with time zone NOT NULL,
    rated_through timestamp with time zone NOT NULL,
    accrued_amount bigint DEFAULT 0 NOT NULL,
    created_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    updated_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    CONSTRAINT metered_rating_watermarks_pkey PRIMARY KEY (merchant_id, customer_id, currency, source, period_from),
    CONSTRAINT metered_rating_watermarks_accrued_nonneg CHECK (accrued_amount >= 0)
);

COMMENT ON TABLE openrails.metered_rating_watermarks IS '#672 per-period metered-rating watermark: cumulative accrued amount + rated-through cutoff per (payer, currency, meter source, period start), so overlapping invoice closes bill each unit of usage exactly once.';
COMMENT ON COLUMN openrails.metered_rating_watermarks.source IS 'Meter accrual source key (metered:<meter>[:rate_card:<id>][:dim:<value>]).';
COMMENT ON COLUMN openrails.metered_rating_watermarks.accrued_amount IS 'Micros already accrued for [period_from, rated_through); the sweep accrues only the delta above this.';

ALTER TABLE openrails.metered_rating_watermarks FORCE ROW LEVEL SECURITY;
ALTER TABLE openrails.metered_rating_watermarks ENABLE ROW LEVEL SECURITY;
CREATE POLICY merchant_isolation ON openrails.metered_rating_watermarks
    USING (merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)
    WITH CHECK (merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid);
GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.metered_rating_watermarks TO openrails_app;

ALTER TABLE ONLY openrails.metered_rating_watermarks
    ADD CONSTRAINT metered_rating_watermarks_merchant_fk FOREIGN KEY (merchant_id) REFERENCES openrails.merchants(id) ON DELETE RESTRICT;
