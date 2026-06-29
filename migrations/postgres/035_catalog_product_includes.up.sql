-- #611: catalog-declared product bundles. This is catalog metadata only:
-- purchase/grant code decides when included products are granted to a customer.
CREATE TABLE openrails.product_includes (
    merchant_id uuid NOT NULL,
    product_id uuid NOT NULL,
    included_product_id uuid NOT NULL,
    created_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    CONSTRAINT product_includes_pkey PRIMARY KEY (merchant_id, product_id, included_product_id),
    CONSTRAINT product_includes_not_self CHECK (product_id <> included_product_id)
);

COMMENT ON TABLE openrails.product_includes IS '#611 catalog bundle includes: parent product grants/owns included catalog products when materialized.';

ALTER TABLE openrails.product_includes FORCE ROW LEVEL SECURITY;
ALTER TABLE openrails.product_includes ENABLE ROW LEVEL SECURITY;
CREATE POLICY merchant_isolation ON openrails.product_includes
    USING (merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)
    WITH CHECK (merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid);
GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.product_includes TO openrails_app;
ALTER TABLE ONLY openrails.product_includes
    ADD CONSTRAINT product_includes_merchant_fk FOREIGN KEY (merchant_id) REFERENCES openrails.merchants(id) ON DELETE RESTRICT;
ALTER TABLE ONLY openrails.product_includes
    ADD CONSTRAINT product_includes_product_fk FOREIGN KEY (product_id) REFERENCES openrails.products(id) ON DELETE CASCADE;
ALTER TABLE ONLY openrails.product_includes
    ADD CONSTRAINT product_includes_included_product_fk FOREIGN KEY (included_product_id) REFERENCES openrails.products(id) ON DELETE CASCADE;

CREATE INDEX idx_product_includes_included_product
    ON openrails.product_includes (merchant_id, included_product_id);
