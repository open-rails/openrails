-- =============================================================================
-- 042 — Cross-tenant platform audit log + break-glass grants (issue #226)
--
-- The managed-hosting PLATFORM superadmin layer is DISTINCT from per-tenant
-- operator admin (#224). These two tables are CROSS-TENANT control-plane state
-- (billing.* but carrying NO tenant_id default and NOT purged by tenant delete):
--
--   * billing.platform_audit       — every superadmin cross-tenant action.
--   * billing.platform_break_glass — time-boxed, justified elevation grants.
--
-- They live OUTSIDE tenant-scoped data on purpose: a platform action against
-- tenant X must survive tenant X's deletion (it is the record that the deletion
-- happened), and a superadmin's break-glass elevation is not owned by any single
-- tenant. The 039 backfill loop only touches the tenant-owned table list, so it
-- never adds a tenant_id to these.
--
-- Additive + backward-compatible.
-- =============================================================================

SET lock_timeout      = '10s';
SET statement_timeout = '300s';

-- -----------------------------------------------------------------------------
-- billing.platform_audit — cross-tenant platform superadmin audit log.
--
-- Append-only. One row per superadmin cross-tenant action. `target_tenant_id`
-- is the tenant acted upon (NULL for platform-wide actions like list/metrics).
-- before_state / after_state capture the change where applicable (JSONB so each
-- action records its own shape). `reason` is required for mutating actions.
-- -----------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS billing.platform_audit (
    id               UUID        PRIMARY KEY DEFAULT uuidv7(),
    actor_user_id    TEXT        NOT NULL,
    actor_org        TEXT,
    action           TEXT        NOT NULL,
    target_tenant_id UUID,
    reason           TEXT,
    before_state     JSONB,
    after_state      JSONB,
    detail           JSONB,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT current_timestamp
);

CREATE INDEX IF NOT EXISTS idx_platform_audit_target
    ON billing.platform_audit (target_tenant_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_platform_audit_actor
    ON billing.platform_audit (actor_user_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_platform_audit_action
    ON billing.platform_audit (action, created_at DESC);

COMMENT ON TABLE billing.platform_audit IS
    'Append-only cross-tenant platform superadmin audit log (issue #226). Records actor, target tenant, action, reason, and before/after state. CROSS-TENANT control-plane state: NOT purged by tenant delete.';

-- -----------------------------------------------------------------------------
-- billing.platform_break_glass — time-boxed break-glass elevation grants.
--
-- A break-glass grant is a justified, expiring elevation for emergency
-- cross-tenant access. It requires a written justification, has an explicit
-- expiry, and is itself audited (a row is also written to platform_audit when
-- granted and when revoked). `revoked_at` lets a grant be ended early.
-- -----------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS billing.platform_break_glass (
    id                UUID        PRIMARY KEY DEFAULT uuidv7(),
    actor_user_id     TEXT        NOT NULL,
    target_tenant_id  UUID,
    justification     TEXT        NOT NULL,
    granted_at        TIMESTAMPTZ NOT NULL DEFAULT current_timestamp,
    expires_at        TIMESTAMPTZ NOT NULL,
    revoked_at        TIMESTAMPTZ,

    CONSTRAINT chk_break_glass_window CHECK (expires_at > granted_at),
    CONSTRAINT chk_break_glass_justified CHECK (length(btrim(justification)) > 0)
);

CREATE INDEX IF NOT EXISTS idx_break_glass_actor
    ON billing.platform_break_glass (actor_user_id, expires_at DESC);
CREATE INDEX IF NOT EXISTS idx_break_glass_active
    ON billing.platform_break_glass (expires_at)
    WHERE revoked_at IS NULL;

COMMENT ON TABLE billing.platform_break_glass IS
    'Time-boxed break-glass elevation grants (issue #226). Each grant carries a written justification and an expiry, and is mirrored into platform_audit. CROSS-TENANT control-plane state.';
