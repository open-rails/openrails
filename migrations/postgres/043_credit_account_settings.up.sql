-- =============================================================================
-- 043 — Credit account spend policy + per-invoker spend limits (issue #237)
--
-- Per-(tenant, owner org, credit_type) billing/spend policy: the "API settings
-- around credits" surface. One row holds the org-level policy (billing mode,
-- daily/monthly spend caps, outstanding ceiling, low-balance threshold,
-- auto-top-up config, default credit expiry) plus the dynamic accrual/dedup
-- fields the money-in workers need (outstanding owed for arrears #241,
-- last_alert_at for low-balance alerts #240, last_topup_at for auto-top-up #239).
--
-- credit_spend_limits holds OPTIONAL per-invoker sub-limits (issue #237
-- per_invoker_caps + #246 invoker granularity): an org can cap a specific OAT
-- ('oat:<key_id>'), member ('user:<id>'), or delegated user ('issuer:sub').
-- The invoker string is matched against credit_transactions.user_id (the ACTOR
-- that caused usage), which the metering caller stamps with the canonical
-- invoker form.
--
-- All money amounts are signed BIGINT cents. NULL cap = "no cap".
-- =============================================================================

SET lock_timeout      = '10s';
SET statement_timeout = '300s';

CREATE TABLE IF NOT EXISTS billing.credit_account_settings (
    id                            UUID PRIMARY KEY,
    tenant_id                     UUID NOT NULL,
    owner_id                      UUID NOT NULL,
    credit_type_id                UUID NOT NULL REFERENCES billing.credit_types(id) ON DELETE CASCADE,

    -- Policy.
    billing_mode                  TEXT    NOT NULL DEFAULT 'prepaid',  -- prepaid | arrears
    max_spend_per_day_cents       BIGINT,                              -- NULL = no daily cap
    max_spend_per_month_cents     BIGINT,                              -- NULL = no monthly cap
    max_outstanding_owed_cents    BIGINT,                              -- arrears credit-risk ceiling (NULL = no ceiling)
    low_balance_threshold_cents   BIGINT,                              -- alert / auto-top-up trigger (NULL = off)
    auto_topup_enabled            BOOLEAN NOT NULL DEFAULT false,
    auto_topup_amount_cents       BIGINT,                              -- amount to purchase per top-up
    auto_topup_payment_method_id  UUID,                                -- billing.payment_methods.id
    default_credit_expiry_days    INTEGER,                             -- e.g. 365 (NULL = non-expiring)
    hard_stop_on_breach           BOOLEAN NOT NULL DEFAULT true,       -- false = warn-only (allow + flag)
    alert_threshold_pct           INTEGER NOT NULL DEFAULT 80,         -- alert at this % of any cap

    -- Dynamic state (driven by the money-in / alert workers).
    outstanding_owed_cents        BIGINT      NOT NULL DEFAULT 0,      -- arrears accrual (#241)
    last_alert_at                 TIMESTAMPTZ,                         -- low-balance alert dedup (#240)
    last_topup_at                 TIMESTAMPTZ,                         -- auto-top-up dedup (#239)

    created_at                    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at                    TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT credit_account_settings_billing_mode_chk
        CHECK (billing_mode IN ('prepaid', 'arrears')),
    CONSTRAINT credit_account_settings_alert_pct_chk
        CHECK (alert_threshold_pct BETWEEN 0 AND 100),
    CONSTRAINT credit_account_settings_owed_nonneg_chk
        CHECK (outstanding_owed_cents >= 0)
);

CREATE UNIQUE INDEX IF NOT EXISTS uq_credit_account_settings_owner_type
    ON billing.credit_account_settings (tenant_id, owner_id, credit_type_id);

COMMENT ON TABLE billing.credit_account_settings IS
    'Per-(tenant, owner org, credit_type) spend policy + money-in config (issue #237). Tensorhub SETS these; OpenRails STORES + ENFORCES them.';

-- -----------------------------------------------------------------------------
-- Optional per-invoker sub-limits (issue #237 per_invoker_caps / #246).
-- -----------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS billing.credit_spend_limits (
    id                         UUID PRIMARY KEY,
    tenant_id                  UUID NOT NULL,
    owner_id                   UUID NOT NULL,
    credit_type_id             UUID NOT NULL REFERENCES billing.credit_types(id) ON DELETE CASCADE,
    invoker                    TEXT NOT NULL,        -- 'oat:<key_id>' | 'user:<id>' | '<issuer>:<sub>'
    max_spend_per_day_cents    BIGINT,
    max_spend_per_month_cents  BIGINT,
    created_at                 TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at                 TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX IF NOT EXISTS uq_credit_spend_limits_owner_invoker
    ON billing.credit_spend_limits (tenant_id, owner_id, credit_type_id, invoker);

COMMENT ON TABLE billing.credit_spend_limits IS
    'Optional per-invoker spend caps under an owner (issue #237/#246). invoker matches credit_transactions.user_id (the actor).';

-- Support the per-invoker windowed spend sum (owner + actor + window).
CREATE INDEX IF NOT EXISTS idx_credit_transactions_owner_actor
    ON billing.credit_transactions (tenant_id, owner_id, credit_type_id, user_id, created_at DESC);
