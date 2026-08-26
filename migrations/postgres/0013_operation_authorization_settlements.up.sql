-- th-005 tx-native settlement evidence for durable operation authorizations.
-- Money still moves only through the existing double-entry ledger; this table
-- binds exact caller-authored settlement retries to those idempotent postings.

SET LOCAL statement_timeout = '60s';
SET LOCAL lock_timeout = '10s';

ALTER TABLE openrails.operation_authorizations
    ADD COLUMN captured_usd_micros bigint NOT NULL DEFAULT 0,
    ADD CONSTRAINT operation_authorizations_captured_nonnegative
        CHECK (captured_usd_micros >= 0),
    ADD CONSTRAINT operation_authorizations_release_has_no_capture
        CHECK (state <> 'released' OR captured_usd_micros = 0);

CREATE TABLE openrails.operation_authorization_settlements (
    merchant_id uuid NOT NULL,
    operation_id text NOT NULL,
    settlement_id text NOT NULL,
    amount_usd_micros bigint NOT NULL,
    settlement_body_bytes bytea NOT NULL,
    settlement_body_digest bytea NOT NULL,
    final boolean NOT NULL,
    final_reference text,
    created_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
    CONSTRAINT operation_authorization_settlements_pkey
        PRIMARY KEY (merchant_id, operation_id, settlement_id),
    CONSTRAINT operation_authorization_settlements_authorization_fk
        FOREIGN KEY (merchant_id, operation_id)
        REFERENCES openrails.operation_authorizations(merchant_id, operation_id)
        ON DELETE RESTRICT,
    CONSTRAINT operation_authorization_settlements_id_present
        CHECK (settlement_id <> '' AND settlement_id = btrim(settlement_id)),
    CONSTRAINT operation_authorization_settlements_id_size
        CHECK (octet_length(settlement_id) <= 255),
    CONSTRAINT operation_authorization_settlements_amount_nonnegative
        CHECK (amount_usd_micros >= 0),
    CONSTRAINT operation_authorization_settlements_body_present
        CHECK (octet_length(settlement_body_bytes) > 0),
    CONSTRAINT operation_authorization_settlements_body_size
        CHECK (octet_length(settlement_body_bytes) <= 65536),
    CONSTRAINT operation_authorization_settlements_digest_shape
        CHECK (octet_length(settlement_body_digest) = 32),
    CONSTRAINT operation_authorization_settlements_digest_matches_body
        CHECK (settlement_body_digest = public.digest(settlement_body_bytes, 'sha256')),
    CONSTRAINT operation_authorization_settlements_final_reference_size
        CHECK (final_reference IS NULL OR octet_length(final_reference) <= 1024),
    CONSTRAINT operation_authorization_settlements_final_shape CHECK (
        (final AND final_reference <> '' AND final_reference = btrim(final_reference))
        OR
        (NOT final AND final_reference IS NULL AND amount_usd_micros > 0)
    )
);

ALTER TABLE ONLY openrails.operation_authorization_settlements FORCE ROW LEVEL SECURITY;

COMMENT ON TABLE openrails.operation_authorization_settlements IS
    'Immutable exact-replay evidence for operation-authorization capture increments. Each row maps deterministically to an existing-ledger capture coordinate; it is not a parallel ledger.';

COMMENT ON COLUMN openrails.operation_authorization_settlements.amount_usd_micros IS
    'Actual incremental provider cost. Cumulative settlement may exceed the authorization and must never be clamped.';

COMMENT ON COLUMN openrails.operation_authorization_settlements.final_reference IS
    'Opaque host-owned proof for a final provider absence, BillingStop, or zero-window conclusion; OpenRails does not interpret it.';

CREATE UNIQUE INDEX idx_operation_authorization_settlements_one_final
    ON openrails.operation_authorization_settlements (merchant_id, operation_id)
    WHERE final;

ALTER TABLE openrails.operation_authorization_settlements ENABLE ROW LEVEL SECURITY;

CREATE POLICY merchant_isolation ON openrails.operation_authorization_settlements
    USING (merchant_id = NULLIF(current_setting('app.merchant_id', true), '')::uuid)
    WITH CHECK (merchant_id = NULLIF(current_setting('app.merchant_id', true), '')::uuid);

GRANT SELECT, INSERT ON TABLE openrails.operation_authorization_settlements TO openrails_app;
GRANT UPDATE (captured_usd_micros) ON TABLE openrails.operation_authorizations TO openrails_app;
