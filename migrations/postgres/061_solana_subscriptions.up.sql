-- =============================================================================
-- 061 — Solana recurring subscriptions on-chain state (issue #255)
--
-- Per-subscriber on-chain state for the recurring Solana subscription flow. This
-- is a DEDICATED table (not subscription metadata) because the state is load-
-- bearing and the hourly pull worker (#256) queries it by (tenant_id, next_pull_at).
--
-- It links 1:1 to a billing.subscriptions row (the canonical lifecycle record;
-- Processor='solana', ProcessorSubscriptionID = subscription_pda). It holds ONLY
-- non-secret, public on-chain data — never a private key (those live in the
-- tenant secret store / Vault, #251).
-- =============================================================================

SET lock_timeout      = '10s';
SET statement_timeout = '300s';

CREATE TABLE IF NOT EXISTS billing.solana_subscriptions (
    id                          UUID        PRIMARY KEY DEFAULT uuidv7(),

    -- Tenant scoping (issue #221/#223). Nullable with a default of the default
    -- tenant, matching the additive pattern of 039_tenant_aware_core.
    tenant_id                   UUID        NOT NULL DEFAULT '00000000-0000-0000-0000-000000000001',

    -- The canonical lifecycle subscription this on-chain state belongs to.
    subscription_id             UUID        NOT NULL
        REFERENCES billing.subscriptions(id) ON DELETE CASCADE,

    -- Public on-chain identities (NEVER a private key).
    subscriber_wallet           TEXT        NOT NULL,   -- the subscriber's wallet
    authority_pda               TEXT        NOT NULL,   -- subscription authority PDA
    subscription_pda            TEXT        NOT NULL,   -- per-subscriber subscription PDA
    plan_pda                    TEXT        NOT NULL,   -- the on-chain plan
    merchant_address            TEXT        NOT NULL,   -- plan owner / cranking wallet (public)
    mint                        TEXT        NOT NULL,   -- stablecoin mint (USDC)

    -- Ghost-plan detection: the on-chain plan's created_at fingerprint snapshotted
    -- at subscribe time. A mismatch on pull means the plan was deleted+recreated.
    plan_created_at_fingerprint BIGINT      NOT NULL,

    -- Billing cadence state, advanced only on a CONFIRMED pull.
    last_pulled_period_start    TIMESTAMPTZ,
    last_signature              TEXT,
    next_pull_at                TIMESTAMPTZ NOT NULL,

    -- Lifecycle of this on-chain record: active | cancelled | expired.
    status                      TEXT        NOT NULL DEFAULT 'active',

    created_at                  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at                  TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- One on-chain record per subscription PDA (idempotent upsert key).
    CONSTRAINT solana_subscriptions_subscription_pda_key UNIQUE (subscription_pda)
);

-- The hourly pull worker's due-query: due rows per tenant.
CREATE INDEX IF NOT EXISTS idx_solana_subscriptions_due
    ON billing.solana_subscriptions (tenant_id, next_pull_at)
    WHERE status = 'active';

-- Lookup by the linked lifecycle subscription.
CREATE INDEX IF NOT EXISTS idx_solana_subscriptions_subscription_id
    ON billing.solana_subscriptions (subscription_id);
