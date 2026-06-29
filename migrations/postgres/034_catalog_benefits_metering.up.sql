-- #594/#599: catalog benefit registries and OpenRails-native metered pricing.
-- These are sidecar tables: no hot checkout/subscription table rewrite, and no
-- provider-owned semantics. Request counters remain in Redis; these rows are
-- durable config/materialized bindings.

CREATE TABLE openrails.catalog_usage_limits (
    merchant_id uuid NOT NULL,
    key text NOT NULL,
    measure text NOT NULL,
    windows jsonb DEFAULT '[]'::jsonb NOT NULL,
    created_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    updated_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    CONSTRAINT catalog_usage_limits_pkey PRIMARY KEY (merchant_id, key),
    CONSTRAINT catalog_usage_limits_key_nonempty CHECK (btrim(key) <> ''),
    CONSTRAINT catalog_usage_limits_measure_nonempty CHECK (btrim(measure) <> ''),
    CONSTRAINT catalog_usage_limits_windows_array CHECK (jsonb_typeof(windows) = 'array')
);

COMMENT ON TABLE openrails.catalog_usage_limits IS '#594 catalog usage-limit registry. Durable config only; Redis/Garnet owns request-time counters.';
COMMENT ON COLUMN openrails.catalog_usage_limits.measure IS 'Host-reported event stream key composed into admission policy; not a money meter.';

ALTER TABLE openrails.catalog_usage_limits FORCE ROW LEVEL SECURITY;
ALTER TABLE openrails.catalog_usage_limits ENABLE ROW LEVEL SECURITY;
CREATE POLICY merchant_isolation ON openrails.catalog_usage_limits
    USING (merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)
    WITH CHECK (merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid);
GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.catalog_usage_limits TO openrails_app;
ALTER TABLE ONLY openrails.catalog_usage_limits
    ADD CONSTRAINT catalog_usage_limits_merchant_fk FOREIGN KEY (merchant_id) REFERENCES openrails.merchants(id) ON DELETE RESTRICT;

CREATE TABLE openrails.product_usage_limit_bindings (
    id uuid NOT NULL,
    merchant_id uuid NOT NULL,
    customer_id uuid NOT NULL,
    usage_limit_key text NOT NULL,
    measure text NOT NULL,
    windows jsonb DEFAULT '[]'::jsonb NOT NULL,
    source_type text NOT NULL,
    source_id uuid,
    product_key text,
    grant_id uuid,
    starts_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    ends_at timestamp with time zone,
    revoked_at timestamp with time zone,
    policy_version bigint DEFAULT 1 NOT NULL,
    created_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    updated_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    CONSTRAINT product_usage_limit_bindings_pkey PRIMARY KEY (id),
    CONSTRAINT product_usage_limit_bindings_key_nonempty CHECK (btrim(usage_limit_key) <> ''),
    CONSTRAINT product_usage_limit_bindings_measure_nonempty CHECK (btrim(measure) <> ''),
    CONSTRAINT product_usage_limit_bindings_windows_array CHECK (jsonb_typeof(windows) = 'array'),
    CONSTRAINT product_usage_limit_bindings_time_check CHECK ((ends_at IS NULL) OR (ends_at > starts_at))
);

COMMENT ON TABLE openrails.product_usage_limit_bindings IS '#594 materialized product-derived usage-limit bindings. Loaded into admission policy; live counters stay in Redis.';
COMMENT ON COLUMN openrails.product_usage_limit_bindings.product_key IS 'Catalog product key/slug whose current benefits were materialized at grant time.';

CREATE INDEX idx_product_usage_limit_bindings_active
    ON openrails.product_usage_limit_bindings (merchant_id, customer_id, measure)
    WHERE revoked_at IS NULL;

ALTER TABLE openrails.product_usage_limit_bindings FORCE ROW LEVEL SECURITY;
ALTER TABLE openrails.product_usage_limit_bindings ENABLE ROW LEVEL SECURITY;
CREATE POLICY merchant_isolation ON openrails.product_usage_limit_bindings
    USING (merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)
    WITH CHECK (merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid);
GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.product_usage_limit_bindings TO openrails_app;
ALTER TABLE ONLY openrails.product_usage_limit_bindings
    ADD CONSTRAINT product_usage_limit_bindings_customer_fk FOREIGN KEY (customer_id) REFERENCES openrails.customers(id) ON DELETE RESTRICT;
ALTER TABLE ONLY openrails.product_usage_limit_bindings
    ADD CONSTRAINT product_usage_limit_bindings_merchant_fk FOREIGN KEY (merchant_id) REFERENCES openrails.merchants(id) ON DELETE RESTRICT;

CREATE TABLE openrails.catalog_meters (
    merchant_id uuid NOT NULL,
    key text NOT NULL,
    kind text NOT NULL,
    created_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    updated_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    CONSTRAINT catalog_meters_pkey PRIMARY KEY (merchant_id, key),
    CONSTRAINT catalog_meters_key_nonempty CHECK (btrim(key) <> ''),
    CONSTRAINT catalog_meters_kind_check CHECK (kind = ANY (ARRAY['counter'::text, 'gauge'::text]))
);

COMMENT ON TABLE openrails.catalog_meters IS '#599 billing meter registry. Meters are billed-later usage streams, distinct from #594 usage limits.';
COMMENT ON COLUMN openrails.catalog_meters.kind IS 'counter = summed events; gauge = time-integrated level samples.';

ALTER TABLE openrails.catalog_meters FORCE ROW LEVEL SECURITY;
ALTER TABLE openrails.catalog_meters ENABLE ROW LEVEL SECURITY;
CREATE POLICY merchant_isolation ON openrails.catalog_meters
    USING (merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)
    WITH CHECK (merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid);
GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.catalog_meters TO openrails_app;
ALTER TABLE ONLY openrails.catalog_meters
    ADD CONSTRAINT catalog_meters_merchant_fk FOREIGN KEY (merchant_id) REFERENCES openrails.merchants(id) ON DELETE RESTRICT;

CREATE TABLE openrails.catalog_price_metered (
    price_id uuid NOT NULL,
    merchant_id uuid NOT NULL,
    meter_key text NOT NULL,
    rate_micros bigint NOT NULL,
    per_units bigint DEFAULT 1 NOT NULL,
    per_seconds bigint,
    created_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    updated_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    CONSTRAINT catalog_price_metered_pkey PRIMARY KEY (price_id),
    CONSTRAINT catalog_price_metered_meter_nonempty CHECK (btrim(meter_key) <> ''),
    CONSTRAINT catalog_price_metered_rate_positive CHECK (rate_micros > 0),
    CONSTRAINT catalog_price_metered_per_units_positive CHECK (per_units >= 1),
    CONSTRAINT catalog_price_metered_per_seconds_positive CHECK ((per_seconds IS NULL) OR (per_seconds > 0))
);

COMMENT ON TABLE openrails.catalog_price_metered IS '#599 optional rating sidecar for OpenRails-native metered prices. cost = aggregate * rate_micros / (per_units * per_seconds for gauges), rounded once.';
COMMENT ON COLUMN openrails.catalog_price_metered.per_seconds IS 'Gauge denominator in seconds; NULL for counter meters.';

ALTER TABLE openrails.catalog_price_metered FORCE ROW LEVEL SECURITY;
ALTER TABLE openrails.catalog_price_metered ENABLE ROW LEVEL SECURITY;
CREATE POLICY merchant_isolation ON openrails.catalog_price_metered
    USING (merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)
    WITH CHECK (merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid);
GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.catalog_price_metered TO openrails_app;
ALTER TABLE ONLY openrails.catalog_price_metered
    ADD CONSTRAINT catalog_price_metered_price_fk FOREIGN KEY (price_id) REFERENCES openrails.prices(id) ON DELETE CASCADE;
ALTER TABLE ONLY openrails.catalog_price_metered
    ADD CONSTRAINT catalog_price_metered_merchant_fk FOREIGN KEY (merchant_id) REFERENCES openrails.merchants(id) ON DELETE RESTRICT;
ALTER TABLE ONLY openrails.catalog_price_metered
    ADD CONSTRAINT catalog_price_metered_meter_fk FOREIGN KEY (merchant_id, meter_key) REFERENCES openrails.catalog_meters(merchant_id, key) ON DELETE RESTRICT;
