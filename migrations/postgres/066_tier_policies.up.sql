-- =============================================================================
-- 066 — Tier policies (issue #298)
--
-- Per-(owner) tier definitions used by the admission check: each tier carries a
-- THROUGHPUT policy (RPM/RPD/TPM/TPD over arbitrary unit types) enforced by the
-- internal/modules/ratelimit limiter. The MONEY axis (balance/credit-limit/caps)
-- stays in credit_account_settings; this table is the throughput half + the
-- per-tier knobs. The richer rolling money-budget windows (#304) extend this.
--
-- RLS (issue #227): exact migration-050 tenant_isolation policy form.
-- =============================================================================

SET lock_timeout      = '10s';
SET statement_timeout = '300s';

CREATE TABLE IF NOT EXISTS billing.tier_policies (
    id             UUID        PRIMARY KEY DEFAULT uuidv7(),
    tenant_id      UUID        NOT NULL,
    -- Owner the policy belongs to. The tenant->owner level uses a reserved
    -- owner sentinel (all-zero uuid) for tenant-wide defaults; owner->actor uses
    -- the real owner id.
    owner_id       UUID        NOT NULL,
    -- Tier name (e.g. "free", "tier_1"). The policy applies to actors at this tier.
    tier           TEXT        NOT NULL,
    -- Throughput policy: {"windows":[{"unit":"request","window_seconds":60,"max":500}, ...]}.
    policy         JSONB       NOT NULL DEFAULT '{}',
    policy_version BIGINT      NOT NULL DEFAULT 1,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

COMMENT ON TABLE billing.tier_policies IS
    'Per-owner tier throughput policies for the admission check (issue #298). MONEY caps stay in credit_account_settings; rolling money budgets are #304.';

CREATE UNIQUE INDEX IF NOT EXISTS uq_tier_policies
    ON billing.tier_policies (tenant_id, owner_id, tier);

ALTER TABLE billing.tier_policies ENABLE ROW LEVEL SECURITY;
ALTER TABLE billing.tier_policies FORCE  ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation ON billing.tier_policies;
CREATE POLICY tenant_isolation ON billing.tier_policies
    USING      (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

GRANT SELECT, INSERT, UPDATE, DELETE ON billing.tier_policies TO openrails_app;
