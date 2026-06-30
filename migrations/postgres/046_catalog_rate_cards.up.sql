ALTER TABLE openrails.catalog_meters
    ALTER COLUMN kind DROP NOT NULL;

ALTER TABLE openrails.catalog_meters
    DROP CONSTRAINT IF EXISTS catalog_meters_kind_check;

ALTER TABLE openrails.catalog_meters
    ADD COLUMN IF NOT EXISTS event_type text,
    ADD COLUMN IF NOT EXISTS value_property text,
    ADD COLUMN IF NOT EXISTS aggregation text,
    ADD COLUMN IF NOT EXISTS unit text,
    ADD COLUMN IF NOT EXISTS group_by jsonb DEFAULT '{}'::jsonb NOT NULL;

ALTER TABLE openrails.catalog_meters
    ADD CONSTRAINT catalog_meters_kind_check
    CHECK ((kind IS NULL) OR (kind = ANY (ARRAY['counter'::text, 'gauge'::text])));

ALTER TABLE openrails.catalog_meters
    ADD CONSTRAINT catalog_meters_aggregation_check
    CHECK ((aggregation IS NULL) OR (aggregation = ANY (ARRAY['sum'::text, 'count'::text, 'max'::text, 'min'::text, 'unique_count'::text, 'latest'::text])));

COMMENT ON COLUMN openrails.catalog_meters.event_type IS '#638 usage event type for rate-card meters; defaults to key when omitted.';
COMMENT ON COLUMN openrails.catalog_meters.value_property IS '#638 JSON/dimension property carrying the numeric quantity to aggregate.';
COMMENT ON COLUMN openrails.catalog_meters.aggregation IS '#638 aggregation mode for rate-card meters.';
COMMENT ON COLUMN openrails.catalog_meters.group_by IS '#638 dimension name -> event metadata/dimension property mapping for matrix pricing.';

CREATE TABLE openrails.catalog_rate_cards (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    merchant_id uuid NOT NULL,
    product_id uuid NOT NULL,
    ordinal integer NOT NULL,
    meter_key text,
    payment_term text DEFAULT 'in_arrears'::text NOT NULL,
    billing_cadence_hours integer,
    filter jsonb DEFAULT '{}'::jsonb NOT NULL,
    allowance jsonb,
    price jsonb NOT NULL,
    created_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    updated_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    CONSTRAINT catalog_rate_cards_pkey PRIMARY KEY (id),
    CONSTRAINT catalog_rate_cards_ordinal_positive CHECK (ordinal >= 1),
    CONSTRAINT catalog_rate_cards_payment_term_check CHECK (payment_term = ANY (ARRAY['in_advance'::text, 'in_arrears'::text])),
    CONSTRAINT catalog_rate_cards_billing_cadence_positive CHECK ((billing_cadence_hours IS NULL) OR (billing_cadence_hours > 0))
);

COMMENT ON TABLE openrails.catalog_rate_cards IS '#638 rate-card sidecars: product usage/flat prices expressed as shared charge-model JSON.';

ALTER TABLE openrails.catalog_rate_cards FORCE ROW LEVEL SECURITY;
ALTER TABLE openrails.catalog_rate_cards ENABLE ROW LEVEL SECURITY;
CREATE POLICY merchant_isolation ON openrails.catalog_rate_cards
    USING (merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)
    WITH CHECK (merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid);
GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.catalog_rate_cards TO openrails_app;

CREATE UNIQUE INDEX uq_catalog_rate_cards_product_ordinal ON openrails.catalog_rate_cards (merchant_id, product_id, ordinal);
CREATE INDEX idx_catalog_rate_cards_meter ON openrails.catalog_rate_cards (merchant_id, meter_key) WHERE meter_key IS NOT NULL;

ALTER TABLE ONLY openrails.catalog_rate_cards
    ADD CONSTRAINT catalog_rate_cards_product_fk FOREIGN KEY (product_id) REFERENCES openrails.products(id) ON DELETE CASCADE;
ALTER TABLE ONLY openrails.catalog_rate_cards
    ADD CONSTRAINT catalog_rate_cards_merchant_fk FOREIGN KEY (merchant_id) REFERENCES openrails.merchants(id) ON DELETE RESTRICT;
ALTER TABLE ONLY openrails.catalog_rate_cards
    ADD CONSTRAINT catalog_rate_cards_meter_fk FOREIGN KEY (merchant_id, meter_key) REFERENCES openrails.catalog_meters(merchant_id, key) ON DELETE RESTRICT;

CREATE TABLE openrails.catalog_credit_purchases (
    product_id uuid NOT NULL,
    merchant_id uuid NOT NULL,
    credit_type text NOT NULL,
    unit text NOT NULL,
    currency text NOT NULL,
    expires_hours integer,
    providers text[] DEFAULT ARRAY[]::text[] NOT NULL,
    input_min bigint DEFAULT 0 NOT NULL,
    input_max bigint DEFAULT 0 NOT NULL,
    round text,
    price jsonb NOT NULL,
    created_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    updated_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    CONSTRAINT catalog_credit_purchases_pkey PRIMARY KEY (product_id),
    CONSTRAINT catalog_credit_purchases_credit_type_nonempty CHECK (btrim(credit_type) <> ''),
    CONSTRAINT catalog_credit_purchases_unit_nonempty CHECK (btrim(unit) <> ''),
    CONSTRAINT catalog_credit_purchases_currency_nonempty CHECK (btrim(currency) <> ''),
    CONSTRAINT catalog_credit_purchases_expires_positive CHECK ((expires_hours IS NULL) OR (expires_hours > 0)),
    CONSTRAINT catalog_credit_purchases_input_nonnegative CHECK ((input_min >= 0) AND (input_max >= 0)),
    CONSTRAINT catalog_credit_purchases_input_order CHECK ((input_max = 0) OR (input_min <= input_max))
);

COMMENT ON TABLE openrails.catalog_credit_purchases IS '#639/#640 variable prepaid credit-purchase sidecars using the shared charge-model JSON.';

ALTER TABLE openrails.catalog_credit_purchases FORCE ROW LEVEL SECURITY;
ALTER TABLE openrails.catalog_credit_purchases ENABLE ROW LEVEL SECURITY;
CREATE POLICY merchant_isolation ON openrails.catalog_credit_purchases
    USING (merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)
    WITH CHECK (merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid);
GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.catalog_credit_purchases TO openrails_app;

CREATE INDEX idx_catalog_credit_purchases_merchant ON openrails.catalog_credit_purchases (merchant_id);

ALTER TABLE ONLY openrails.catalog_credit_purchases
    ADD CONSTRAINT catalog_credit_purchases_product_fk FOREIGN KEY (product_id) REFERENCES openrails.products(id) ON DELETE CASCADE;
ALTER TABLE ONLY openrails.catalog_credit_purchases
    ADD CONSTRAINT catalog_credit_purchases_merchant_fk FOREIGN KEY (merchant_id) REFERENCES openrails.merchants(id) ON DELETE RESTRICT;
