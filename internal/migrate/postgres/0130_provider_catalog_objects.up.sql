-- Repair: none-needed. The observed catalog starts empty and fills on the next
-- pull or by-id read. Dropping psp_imports.products is one-way: plans are now
-- mapped by creating provider prices, which earlier imports keep.
CREATE TABLE billing.provider_catalog_objects (
    merchant_id uuid NOT NULL,
    psp_id uuid NOT NULL,
    kind text NOT NULL,
    ref text NOT NULL,
    product_ref text,
    name text,
    active boolean NOT NULL,
    deleted_at timestamptz,
    amount text,
    currency text,
    billing_interval_hours integer,
    billing_interval_months integer,
    evidence jsonb NOT NULL,
    observed_at timestamptz NOT NULL,
    observed_via text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT provider_catalog_objects_pkey PRIMARY KEY (merchant_id, psp_id, kind, ref),
    CONSTRAINT provider_catalog_objects_psp_id_fkey FOREIGN KEY (merchant_id, psp_id) REFERENCES billing.psps(merchant_id, id) ON DELETE RESTRICT,
    CONSTRAINT provider_catalog_objects_kind_check CHECK (kind IN ('product', 'price')),
    CONSTRAINT provider_catalog_objects_ref_check CHECK (ref <> '' AND octet_length(ref) <= 256),
    CONSTRAINT provider_catalog_objects_text_check CHECK ((product_ref IS NULL OR product_ref <> '') AND (name IS NULL OR name <> '')),
    CONSTRAINT provider_catalog_objects_terms_check CHECK (kind = 'price'
        OR product_ref IS NULL AND amount IS NULL AND currency IS NULL AND billing_interval_hours IS NULL AND billing_interval_months IS NULL),
    CONSTRAINT provider_catalog_objects_amount_check CHECK (amount IS NULL OR amount ~ '^[0-9]+(\.[0-9]+)?$'),
    CONSTRAINT provider_catalog_objects_currency_check CHECK (currency IS NULL OR currency ~ '^[A-Z][A-Z0-9]{2,9}$'),
    CONSTRAINT provider_catalog_objects_interval_check CHECK (billing_interval_hours IS NULL OR billing_interval_months IS NULL),
    CONSTRAINT provider_catalog_objects_interval_positive_check CHECK ((billing_interval_hours IS NULL OR billing_interval_hours > 0) AND (billing_interval_months IS NULL OR billing_interval_months > 0)),
    CONSTRAINT provider_catalog_objects_observed_via_check CHECK (observed_via IN ('pull', 'webhook', 'read')),
    CONSTRAINT provider_catalog_objects_evidence_check CHECK (jsonb_typeof(evidence) = 'object')
);
COMMENT ON TABLE billing.provider_catalog_objects IS 'What a PSP''s catalog says, as OpenRails last observed it: Stripe products and prices (active or not) and NMI plans (prices). Provider truth only, never written by a person: pulls, webhooks and by-id reads upsert it, the latest observation winning. amount is the decimal major-unit amount the provider states; currency is NULL where the provider names none (NMI). deleted_at is set only when a by-id read finds the object gone. A provider price is mapped by a provider price bound to it.';

ALTER TABLE billing.psp_import_records
    ADD COLUMN manual_price_id uuid,
    ADD CONSTRAINT psp_import_records_manual_price_id_fkey FOREIGN KEY (merchant_id, manual_price_id) REFERENCES billing.prices(merchant_id, id) ON DELETE RESTRICT,
    ADD CONSTRAINT psp_import_records_manual_price_check CHECK (manual_price_id IS NULL OR kind = 'subscription');
COMMENT ON COLUMN billing.psp_import_records.manual_price_id IS 'The provider price the merchant picked by hand for a subscription whose provider names no plan (CCBill); it wins over matching its rebill.';

ALTER TABLE billing.psp_imports DROP COLUMN products;
COMMENT ON TABLE billing.psp_imports IS 'One read-only pull of a PSP''s catalog, customers, subscriptions and transaction history, and the merchant''s rules for linking them to customers. history_through is how far the transaction history has been read. Deleting the import deletes its records.';
