SET lock_timeout      = '10s';
SET statement_timeout = '300s';

CREATE TABLE IF NOT EXISTS billing.linked_wallets (
    id                    UUID        PRIMARY KEY DEFAULT uuidv7(),
    tenant_id             UUID        NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001',
    tenant_subject_id     UUID        NOT NULL REFERENCES billing.tenant_subjects(id) ON DELETE CASCADE,
    chain                 TEXT        NOT NULL,
    address               TEXT        NOT NULL,
    verification_provider TEXT        NOT NULL,
    verified_at           TIMESTAMPTZ NOT NULL,
    display_name          TEXT,
    metadata              JSONB       NOT NULL DEFAULT '{}'::jsonb,
    created_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at            TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT linked_wallets_chain_address_nonempty
        CHECK (btrim(chain) <> '' AND btrim(address) <> ''),
    CONSTRAINT linked_wallets_unique_subject_chain
        UNIQUE (tenant_id, tenant_subject_id, chain),
    CONSTRAINT linked_wallets_unique_chain_address
        UNIQUE (tenant_id, chain, address)
);

CREATE INDEX IF NOT EXISTS idx_linked_wallets_tenant_subject
    ON billing.linked_wallets (tenant_id, tenant_subject_id);

COMMENT ON TABLE billing.linked_wallets IS
    'Verified user wallet links for browser self-service billing identity. The wallet must come from trusted delegated-token claims, not request body input.';
