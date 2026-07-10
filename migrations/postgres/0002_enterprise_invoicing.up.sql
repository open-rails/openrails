-- #798: enterprise arrears invoicing primitives.

-- Per-payer (negotiated) rate cards: a customer_id-scoped card replaces the
-- merchant-default card for the same meter_key when rating that payer.
-- Manifest sidecar sync only owns customer_id IS NULL rows.
ALTER TABLE openrails.catalog_rate_cards
    ADD COLUMN customer_id uuid,
    ALTER COLUMN product_id DROP NOT NULL;

ALTER TABLE ONLY openrails.catalog_rate_cards
    ADD CONSTRAINT catalog_rate_cards_customer_fk FOREIGN KEY (customer_id) REFERENCES openrails.customers(id) ON DELETE CASCADE;

ALTER TABLE ONLY openrails.catalog_rate_cards
    ADD CONSTRAINT catalog_rate_cards_product_scope_chk CHECK ((customer_id IS NOT NULL) OR (product_id IS NOT NULL));

DROP INDEX openrails.uq_catalog_rate_cards_meter;

CREATE UNIQUE INDEX uq_catalog_rate_cards_meter ON openrails.catalog_rate_cards USING btree (merchant_id, meter_key) WHERE ((meter_key IS NOT NULL) AND (customer_id IS NULL));

CREATE UNIQUE INDEX uq_catalog_rate_cards_payer_meter ON openrails.catalog_rate_cards USING btree (merchant_id, customer_id, meter_key) WHERE ((meter_key IS NOT NULL) AND (customer_id IS NOT NULL));

COMMENT ON COLUMN openrails.catalog_rate_cards.customer_id IS '#798 negotiated per-payer override: when set, this card replaces the merchant-default card for the same meter_key when rating that payer.';

-- Enterprise invoice document fields + manual-remittance collection.
ALTER TABLE openrails.invoices
    ADD COLUMN po_number text,
    ADD COLUMN tax jsonb DEFAULT '{}'::jsonb NOT NULL,
    ADD COLUMN billing_contacts jsonb DEFAULT '[]'::jsonb NOT NULL,
    ADD COLUMN memo text;

ALTER TABLE openrails.invoices DROP CONSTRAINT invoices_collection_method_check;

ALTER TABLE openrails.invoices
    ADD CONSTRAINT invoices_collection_method_check CHECK ((collection_method = ANY (ARRAY['charge_automatically'::text, 'send_invoice'::text])));

COMMENT ON COLUMN openrails.invoices.po_number IS '#798 purchase-order reference snapshotted from the payer invoice profile at finalize.';
COMMENT ON COLUMN openrails.invoices.tax IS '#798 tax document fields (tax id, jurisdiction, rates) snapshotted from the payer invoice profile at finalize. Host-defined shape.';
COMMENT ON COLUMN openrails.invoices.billing_contacts IS '#798 billing contacts ([{name,email}]) snapshotted from the payer invoice profile at finalize.';

-- Per-payer invoice profile: net-N credit terms, collection method and the
-- document fields snapshotted onto every invoice at finalize.
CREATE TABLE openrails.customer_invoice_profiles (
    merchant_id uuid NOT NULL,
    customer_id uuid NOT NULL,
    net_terms_days integer DEFAULT 0 NOT NULL,
    collection_method text DEFAULT 'charge_automatically'::text NOT NULL,
    po_number text,
    tax jsonb DEFAULT '{}'::jsonb NOT NULL,
    billing_contacts jsonb DEFAULT '[]'::jsonb NOT NULL,
    memo text,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT customer_invoice_profiles_net_terms_chk CHECK ((net_terms_days >= 0)),
    CONSTRAINT customer_invoice_profiles_collection_method_chk CHECK ((collection_method = ANY (ARRAY['charge_automatically'::text, 'send_invoice'::text])))
);

ALTER TABLE ONLY openrails.customer_invoice_profiles
    ADD CONSTRAINT customer_invoice_profiles_pkey PRIMARY KEY (merchant_id, customer_id);

ALTER TABLE ONLY openrails.customer_invoice_profiles
    ADD CONSTRAINT customer_invoice_profiles_merchant_fk FOREIGN KEY (merchant_id) REFERENCES openrails.merchants(id) ON DELETE RESTRICT;

ALTER TABLE ONLY openrails.customer_invoice_profiles
    ADD CONSTRAINT customer_invoice_profiles_customer_fk FOREIGN KEY (customer_id) REFERENCES openrails.customers(id) ON DELETE CASCADE;

ALTER TABLE openrails.customer_invoice_profiles ENABLE ROW LEVEL SECURITY;
ALTER TABLE ONLY openrails.customer_invoice_profiles FORCE ROW LEVEL SECURITY;

CREATE POLICY merchant_isolation ON openrails.customer_invoice_profiles USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.customer_invoice_profiles TO openrails_app;

COMMENT ON TABLE openrails.customer_invoice_profiles IS '#798 per-payer enterprise invoicing profile: net-N terms, collection method (charge_automatically | send_invoice for manual remittance) and document fields (PO, tax, contacts) snapshotted onto invoices at finalize.';
