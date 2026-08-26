-- th-005: a provider operation reserves real OpenRails account capacity in the
-- same caller-owned transaction that commits the provider obligation. This is
-- durable financial state linked to the existing customer-balance ledger
-- account; it is deliberately not a second ledger and never expires by time.

SET LOCAL statement_timeout = '60s';
SET LOCAL lock_timeout = '10s';

-- RLS is not a substitute for tenant-consistent foreign keys: app inserts can
-- otherwise name their own merchant_id while referencing another merchant's
-- globally keyed ledger account.
ALTER TABLE ONLY openrails.ledger_accounts
    ADD CONSTRAINT ledger_accounts_merchant_id_id_key UNIQUE (merchant_id, id);

CREATE TABLE openrails.operation_authorizations (
    operation_id text NOT NULL,
    merchant_id uuid NOT NULL,
    payer_id uuid NOT NULL,
    record_owner text NOT NULL,
    ledger_account_id uuid NOT NULL,
    authorized_usd_micros bigint NOT NULL,
    claim_reference text NOT NULL,
    authorization_body_bytes bytea NOT NULL,
    authorization_body_digest bytea NOT NULL,
    state text NOT NULL DEFAULT 'open',
    terminal_reference text,
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    released_at timestamptz,
    settled_at timestamptz,
    CONSTRAINT operation_authorizations_pkey
        PRIMARY KEY (merchant_id, operation_id),
    CONSTRAINT operation_authorizations_operation_id_present
        CHECK (operation_id <> '' AND operation_id = btrim(operation_id)),
    CONSTRAINT operation_authorizations_operation_id_size
        CHECK (octet_length(operation_id) <= 255),
    CONSTRAINT operation_authorizations_record_owner_present
        CHECK (record_owner <> '' AND record_owner = btrim(record_owner)),
    CONSTRAINT operation_authorizations_record_owner_size
        CHECK (octet_length(record_owner) <= 255),
    CONSTRAINT operation_authorizations_claim_reference_present
        CHECK (claim_reference <> '' AND claim_reference = btrim(claim_reference)),
    CONSTRAINT operation_authorizations_claim_reference_size
        CHECK (octet_length(claim_reference) <= 1024),
    CONSTRAINT operation_authorizations_amount_positive
        CHECK (authorized_usd_micros > 0),
    CONSTRAINT operation_authorizations_body_present
        CHECK (octet_length(authorization_body_bytes) > 0),
    CONSTRAINT operation_authorizations_body_size
        CHECK (octet_length(authorization_body_bytes) <= 65536),
    CONSTRAINT operation_authorizations_digest_shape
        CHECK (octet_length(authorization_body_digest) = 32),
    CONSTRAINT operation_authorizations_digest_matches_body
        CHECK (authorization_body_digest = public.digest(authorization_body_bytes, 'sha256')),
    CONSTRAINT operation_authorizations_state_check
        CHECK (state IN ('open', 'released', 'settled')),
    CONSTRAINT operation_authorizations_terminal_reference_size
        CHECK (terminal_reference IS NULL OR octet_length(terminal_reference) <= 1024),
    CONSTRAINT operation_authorizations_terminal_shape CHECK (
        (state = 'open' AND terminal_reference IS NULL AND released_at IS NULL AND settled_at IS NULL)
        OR
        (state = 'released' AND terminal_reference <> '' AND released_at IS NOT NULL AND settled_at IS NULL)
        OR
        (state = 'settled' AND terminal_reference <> '' AND released_at IS NULL AND settled_at IS NOT NULL)
    ),
    CONSTRAINT operation_authorizations_merchant_fk
        FOREIGN KEY (merchant_id) REFERENCES openrails.merchants(id) ON DELETE RESTRICT,
    CONSTRAINT operation_authorizations_payer_fk
        FOREIGN KEY (merchant_id, payer_id)
        REFERENCES openrails.customers(merchant_id, id) ON DELETE RESTRICT,
    CONSTRAINT operation_authorizations_ledger_account_fk
        FOREIGN KEY (merchant_id, ledger_account_id)
        REFERENCES openrails.ledger_accounts(merchant_id, id) ON DELETE RESTRICT
);

ALTER TABLE ONLY openrails.operation_authorizations FORCE ROW LEVEL SECURITY;

COMMENT ON TABLE openrails.operation_authorizations IS
    'Merchant-scoped durable th-005 financial reservations for exact provider-operation bodies. Open rows reserve USD-micro capacity against the linked customer_balance ledger account; they are not ledger movements and never TTL-expire.';

COMMENT ON COLUMN openrails.operation_authorizations.authorization_body_bytes IS
    'Exact canonical bytes authored by the embedding host. OpenRails binds them byte-for-byte but does not interpret their format.';

COMMENT ON COLUMN openrails.operation_authorizations.authorization_body_digest IS
    'Caller-bound SHA-256 of authorization_body_bytes, also rechecked by the database.';

CREATE INDEX idx_operation_authorizations_open_capacity
    ON openrails.operation_authorizations (merchant_id, ledger_account_id)
    WHERE state = 'open';

CREATE INDEX idx_operation_authorizations_payer
    ON openrails.operation_authorizations (merchant_id, payer_id, created_at DESC);

ALTER TABLE openrails.operation_authorizations ENABLE ROW LEVEL SECURITY;

CREATE POLICY merchant_isolation ON openrails.operation_authorizations
    USING (merchant_id = NULLIF(current_setting('app.merchant_id', true), '')::uuid)
    WITH CHECK (merchant_id = NULLIF(current_setting('app.merchant_id', true), '')::uuid);

GRANT SELECT, INSERT ON TABLE openrails.operation_authorizations TO openrails_app;
GRANT UPDATE (state, terminal_reference, released_at, settled_at)
    ON TABLE openrails.operation_authorizations TO openrails_app;
