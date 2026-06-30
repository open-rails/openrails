-- #643: per-customer minimum-spend commitment. A customer commits to spend at
-- least amount_micros per billing period in a currency; at a periodic invoice
-- close, if rated usage falls short, a true-up line brings the invoice up to it.
-- This is a billing-relationship contract, NOT a per-line pricing floor (#642).

CREATE TABLE openrails.customer_minimum_spend (
    merchant_id uuid NOT NULL,
    customer_id uuid NOT NULL,
    currency text NOT NULL,
    amount_micros bigint NOT NULL,
    created_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    updated_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    CONSTRAINT customer_minimum_spend_pkey PRIMARY KEY (merchant_id, customer_id, currency),
    CONSTRAINT customer_minimum_spend_amount_positive CHECK (amount_micros > 0)
);

COMMENT ON TABLE openrails.customer_minimum_spend IS '#643 per-customer per-currency minimum-spend commitment; trues-up at periodic invoice close.';

ALTER TABLE openrails.customer_minimum_spend FORCE ROW LEVEL SECURITY;
ALTER TABLE openrails.customer_minimum_spend ENABLE ROW LEVEL SECURITY;
CREATE POLICY merchant_isolation ON openrails.customer_minimum_spend
    USING (merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)
    WITH CHECK (merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid);
GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.customer_minimum_spend TO openrails_app;

ALTER TABLE ONLY openrails.customer_minimum_spend
    ADD CONSTRAINT customer_minimum_spend_merchant_fk FOREIGN KEY (merchant_id) REFERENCES openrails.merchants(id) ON DELETE RESTRICT;
ALTER TABLE ONLY openrails.customer_minimum_spend
    ADD CONSTRAINT customer_minimum_spend_customer_fk FOREIGN KEY (customer_id) REFERENCES openrails.customers(id) ON DELETE CASCADE;
