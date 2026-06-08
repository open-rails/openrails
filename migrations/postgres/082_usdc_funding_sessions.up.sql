SET lock_timeout      = '10s';
SET statement_timeout = '300s';

CREATE TABLE IF NOT EXISTS billing.usdc_funding_sessions (
    id                  UUID        PRIMARY KEY DEFAULT uuidv7(),
    tenant_id           UUID        NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001',
    tenant_subject_id   UUID        NOT NULL REFERENCES billing.tenant_subjects(id) ON DELETE CASCADE,
    checkout_session_id UUID        REFERENCES billing.checkout_sessions(id) ON DELETE SET NULL,
    provider            TEXT        NOT NULL,
    wallet_address      TEXT        NOT NULL,
    asset               TEXT        NOT NULL,
    network             TEXT        NOT NULL,
    requested_amount    TEXT        NOT NULL,
    provider_session_id TEXT,
    provider_url        TEXT        NOT NULL,
    status              TEXT        NOT NULL,
    return_url          TEXT,
    idempotency_key     TEXT,
    metadata            JSONB       NOT NULL DEFAULT '{}'::jsonb,
    last_checked_at     TIMESTAMPTZ,
    expires_at          TIMESTAMPTZ,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT usdc_funding_sessions_provider_valid
        CHECK (provider IN ('robinhood', 'coinbase')),
    CONSTRAINT usdc_funding_sessions_asset_valid
        CHECK (asset = 'USDC'),
    CONSTRAINT usdc_funding_sessions_status_valid
        CHECK (status IN ('created', 'opened', 'pending_provider', 'pending_settlement', 'funded', 'failed', 'expired', 'cancelled')),
    CONSTRAINT usdc_funding_sessions_nonempty
        CHECK (
            btrim(wallet_address) <> ''
            AND btrim(network) <> ''
            AND btrim(requested_amount) <> ''
            AND btrim(provider_url) <> ''
        )
);

CREATE INDEX IF NOT EXISTS idx_usdc_funding_sessions_tenant_subject
    ON billing.usdc_funding_sessions (tenant_id, tenant_subject_id, created_at DESC);

CREATE INDEX IF NOT EXISTS idx_usdc_funding_sessions_checkout
    ON billing.usdc_funding_sessions (tenant_id, checkout_session_id)
    WHERE checkout_session_id IS NOT NULL;

CREATE INDEX IF NOT EXISTS idx_usdc_funding_sessions_provider_session
    ON billing.usdc_funding_sessions (provider, provider_session_id)
    WHERE provider_session_id IS NOT NULL;

CREATE UNIQUE INDEX IF NOT EXISTS idx_usdc_funding_sessions_idempotency
    ON billing.usdc_funding_sessions (tenant_id, tenant_subject_id, idempotency_key)
    WHERE idempotency_key IS NOT NULL AND btrim(idempotency_key) <> '';

COMMENT ON TABLE billing.usdc_funding_sessions IS
    'External Robinhood/Coinbase handoffs that fund USDC into a user self-custody wallet before normal OpenRails wallet checkout. Return from provider is not proof of funding.';
