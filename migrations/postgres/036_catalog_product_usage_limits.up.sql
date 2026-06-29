-- #617: catalog-declared product usage-limit memberships.
-- catalog_usage_limits defines the reusable limit; this table attaches those
-- limit keys to products so grants can materialize customer bindings.
CREATE TABLE openrails.product_usage_limits (
    merchant_id uuid NOT NULL,
    product_id uuid NOT NULL,
    usage_limit_key text NOT NULL,
    created_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    CONSTRAINT product_usage_limits_pkey PRIMARY KEY (merchant_id, product_id, usage_limit_key),
    CONSTRAINT product_usage_limits_key_nonempty CHECK (btrim(usage_limit_key) <> '')
);

COMMENT ON TABLE openrails.product_usage_limits IS '#617 catalog product usage-limit memberships. Grants materialize these into customer product_usage_limit_bindings.';

ALTER TABLE openrails.product_usage_limits FORCE ROW LEVEL SECURITY;
ALTER TABLE openrails.product_usage_limits ENABLE ROW LEVEL SECURITY;
CREATE POLICY merchant_isolation ON openrails.product_usage_limits
    USING (merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)
    WITH CHECK (merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid);
GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.product_usage_limits TO openrails_app;
ALTER TABLE ONLY openrails.product_usage_limits
    ADD CONSTRAINT product_usage_limits_merchant_fk FOREIGN KEY (merchant_id) REFERENCES openrails.merchants(id) ON DELETE RESTRICT;
ALTER TABLE ONLY openrails.product_usage_limits
    ADD CONSTRAINT product_usage_limits_product_fk FOREIGN KEY (product_id) REFERENCES openrails.products(id) ON DELETE CASCADE;
ALTER TABLE ONLY openrails.product_usage_limits
    ADD CONSTRAINT product_usage_limits_limit_fk FOREIGN KEY (merchant_id, usage_limit_key) REFERENCES openrails.catalog_usage_limits(merchant_id, key) ON DELETE CASCADE;

CREATE INDEX idx_product_usage_limits_key
    ON openrails.product_usage_limits (merchant_id, usage_limit_key);
