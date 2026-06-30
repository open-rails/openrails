CREATE TABLE IF NOT EXISTS openrails.catalog_credit_balances (
    merchant_id uuid NOT NULL,
    key text NOT NULL,
    unit text NOT NULL,
    expires_hours integer,
    created_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    updated_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    CONSTRAINT catalog_credit_balances_pkey PRIMARY KEY (merchant_id, key),
    CONSTRAINT catalog_credit_balances_key_nonempty CHECK (btrim(key) <> ''),
    CONSTRAINT catalog_credit_balances_unit_nonempty CHECK (btrim(unit) <> ''),
    CONSTRAINT catalog_credit_balances_expires_positive CHECK ((expires_hours IS NULL) OR (expires_hours > 0))
);

ALTER TABLE openrails.catalog_credit_balances FORCE ROW LEVEL SECURITY;
ALTER TABLE openrails.catalog_credit_balances ENABLE ROW LEVEL SECURITY;
CREATE POLICY merchant_isolation ON openrails.catalog_credit_balances
    USING (merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)
    WITH CHECK (merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid);
GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.catalog_credit_balances TO openrails_app;

ALTER TABLE ONLY openrails.catalog_credit_balances
    ADD CONSTRAINT catalog_credit_balances_merchant_fk FOREIGN KEY (merchant_id) REFERENCES openrails.merchants(id) ON DELETE RESTRICT;

CREATE TABLE IF NOT EXISTS openrails.catalog_credit_purchase_prices (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    merchant_id uuid NOT NULL,
    product_id uuid NOT NULL,
    ordinal integer NOT NULL,
    credit_key text NOT NULL,
    currency text NOT NULL,
    providers text[] DEFAULT ARRAY[]::text[] NOT NULL,
    input_min bigint DEFAULT 0 NOT NULL,
    input_max bigint DEFAULT 0 NOT NULL,
    round text,
    price jsonb NOT NULL,
    created_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    updated_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    CONSTRAINT catalog_credit_purchase_prices_pkey PRIMARY KEY (id),
    CONSTRAINT catalog_credit_purchase_prices_ordinal_positive CHECK (ordinal >= 1),
    CONSTRAINT catalog_credit_purchase_prices_currency_nonempty CHECK (btrim(currency) <> ''),
    CONSTRAINT catalog_credit_purchase_prices_input_nonnegative CHECK ((input_min >= 0) AND (input_max >= 0)),
    CONSTRAINT catalog_credit_purchase_prices_input_order CHECK ((input_max = 0) OR (input_min <= input_max))
);

ALTER TABLE openrails.catalog_credit_purchase_prices FORCE ROW LEVEL SECURITY;
ALTER TABLE openrails.catalog_credit_purchase_prices ENABLE ROW LEVEL SECURITY;
CREATE POLICY merchant_isolation ON openrails.catalog_credit_purchase_prices
    USING (merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)
    WITH CHECK (merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid);
GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.catalog_credit_purchase_prices TO openrails_app;

CREATE UNIQUE INDEX IF NOT EXISTS uq_catalog_credit_purchase_prices_product_ordinal
    ON openrails.catalog_credit_purchase_prices (merchant_id, product_id, ordinal);

ALTER TABLE ONLY openrails.catalog_credit_purchase_prices
    ADD CONSTRAINT catalog_credit_purchase_prices_product_fk FOREIGN KEY (product_id) REFERENCES openrails.products(id) ON DELETE CASCADE;
ALTER TABLE ONLY openrails.catalog_credit_purchase_prices
    ADD CONSTRAINT catalog_credit_purchase_prices_merchant_fk FOREIGN KEY (merchant_id) REFERENCES openrails.merchants(id) ON DELETE RESTRICT;
ALTER TABLE ONLY openrails.catalog_credit_purchase_prices
    ADD CONSTRAINT catalog_credit_purchase_prices_balance_fk FOREIGN KEY (merchant_id, credit_key) REFERENCES openrails.catalog_credit_balances(merchant_id, key) ON DELETE RESTRICT;

INSERT INTO openrails.catalog_credit_balances (merchant_id, key, unit, expires_hours)
SELECT DISTINCT merchant_id, credit_type, unit, expires_hours
FROM openrails.catalog_credit_purchases
ON CONFLICT (merchant_id, key) DO NOTHING;

INSERT INTO openrails.catalog_credit_purchase_prices
    (merchant_id, product_id, ordinal, credit_key, currency, providers, input_min, input_max, round, price)
SELECT merchant_id, product_id, 1, credit_type, currency, providers, input_min, input_max, round, price
FROM openrails.catalog_credit_purchases
ON CONFLICT DO NOTHING;
