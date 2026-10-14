-- Repair: none-needed New tables only; nothing existing is read or rewritten.
-- A PSP import pulls a merchant's subscribers from one PSP (read only), stages
-- each provider object with its identity facts, links it to a customer by the
-- merchant's rules and mappings, and lands the linked ones through the billing
-- import. Unmatched objects stay staged and listed.
CREATE TABLE billing.psp_imports (
    merchant_id uuid NOT NULL,
    id uuid DEFAULT uuidv7() NOT NULL,
    psp_id uuid NOT NULL,
    rail text NOT NULL,
    status text NOT NULL,
    history_since timestamptz,
    history_through timestamptz,
    pulled_at timestamptz,
    applied_at timestamptz,
    error text,
    rules jsonb NOT NULL,
    products jsonb NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT psp_imports_pkey PRIMARY KEY (merchant_id, id),
    CONSTRAINT psp_imports_psp_id_rail_fkey FOREIGN KEY (merchant_id, psp_id, rail) REFERENCES billing.psps(merchant_id, id, rail) ON DELETE RESTRICT,
    CONSTRAINT psp_imports_status_check CHECK (status IN ('pulling', 'pulled', 'applying', 'applied', 'failed')),
    CONSTRAINT psp_imports_error_check CHECK ((status = 'failed') = (error IS NOT NULL) AND (error IS NULL OR error <> '')),
    CONSTRAINT psp_imports_rules_check CHECK (jsonb_typeof(rules) = 'array'),
    CONSTRAINT psp_imports_products_check CHECK (jsonb_typeof(products) = 'array')
);
CREATE INDEX psp_imports_created_at_id_idx ON billing.psp_imports (merchant_id, created_at DESC, id DESC);
COMMENT ON TABLE billing.psp_imports IS 'One read-only pull of a PSP''s customers, subscriptions and transaction history, the merchant''s rules for linking them to customers, and the products its plans grant. history_through is how far the transaction history has been read. Deleting the import deletes its records.';

CREATE TABLE billing.psp_import_records (
    merchant_id uuid NOT NULL,
    import_id uuid NOT NULL,
    id uuid DEFAULT uuidv7() NOT NULL,
    kind text NOT NULL,
    ref text NOT NULL,
    customer_ref text,
    subscription_ref text,
    occurred_at timestamptz,
    facts jsonb NOT NULL,
    data jsonb NOT NULL,
    state text NOT NULL,
    customer_id uuid,
    linked_via text,
    manual_customer_id uuid,
    reason text,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT psp_import_records_pkey PRIMARY KEY (merchant_id, id),
    CONSTRAINT psp_import_records_ref_key UNIQUE (merchant_id, import_id, kind, ref),
    CONSTRAINT psp_import_records_import_id_fkey FOREIGN KEY (merchant_id, import_id) REFERENCES billing.psp_imports(merchant_id, id) ON DELETE CASCADE,
    CONSTRAINT psp_import_records_kind_check CHECK (kind IN ('customer', 'subscription', 'transaction')),
    CONSTRAINT psp_import_records_ref_check CHECK (ref <> '' AND octet_length(ref) <= 256),
    CONSTRAINT psp_import_records_refs_check CHECK ((customer_ref IS NULL OR customer_ref <> '') AND (subscription_ref IS NULL OR subscription_ref <> '')),
    CONSTRAINT psp_import_records_facts_check CHECK (jsonb_typeof(facts) = 'object'),
    CONSTRAINT psp_import_records_data_check CHECK (jsonb_typeof(data) = 'object'),
    CONSTRAINT psp_import_records_state_check CHECK (
        state IN ('unmatched', 'conflict') AND customer_id IS NULL AND linked_via IS NULL
        OR state IN ('linked', 'imported', 'blocked') AND customer_id IS NOT NULL AND linked_via IS NOT NULL AND linked_via <> ''),
    CONSTRAINT psp_import_records_reason_check CHECK ((reason IS NULL OR reason <> '') AND (state NOT IN ('conflict', 'blocked') OR reason IS NOT NULL))
);
CREATE INDEX psp_import_records_state_idx ON billing.psp_import_records (merchant_id, import_id, state, kind, id);
CREATE INDEX psp_import_records_kind_idx ON billing.psp_import_records (merchant_id, import_id, kind, id);
COMMENT ON TABLE billing.psp_import_records IS 'A provider object a PSP import staged: a customer (vault) with its cards, a subscription or a transaction, with the identity facts the provider holds (email, username, order id, vault ids). linked names the customer a rule, a mapping, the object it belongs to or the merchant linked it to; imported once the billing import landed it; blocked says why it did not land. Deleted with its import.';
COMMENT ON COLUMN billing.psp_import_records.manual_customer_id IS 'The customer the merchant linked this object to by hand; it wins over every rule.';

CREATE TABLE billing.psp_import_mappings (
    merchant_id uuid NOT NULL,
    mapping text NOT NULL,
    key text NOT NULL,
    customer_id uuid NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT psp_import_mappings_pkey PRIMARY KEY (merchant_id, mapping, key),
    CONSTRAINT psp_import_mappings_merchant_id_fkey FOREIGN KEY (merchant_id) REFERENCES billing.merchants(id) ON DELETE RESTRICT,
    CONSTRAINT psp_import_mappings_mapping_check CHECK (mapping ~ '^[a-z][a-z0-9_.-]{0,63}$'),
    CONSTRAINT psp_import_mappings_key_check CHECK (key <> '' AND octet_length(key) <= 256)
);
CREATE INDEX psp_import_mappings_customer_idx ON billing.psp_import_mappings (merchant_id, customer_id);
COMMENT ON TABLE billing.psp_import_mappings IS 'A merchant''s own identifiers mapped to its customers (a legacy user id, a legacy order id), named by mapping. PSP import rules read a provider fact through one.';
