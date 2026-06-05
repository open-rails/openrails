-- =============================================================================
-- 068 — Rolling-window money-budget reservations (issue #304)
--
-- A delegated user (actor) under an owner org is capped to a money budget over
-- one or more ROLLING windows (e.g. "$2 per 4h, $5 per week"). Each reservation
-- row is one in-flight or settled charge against those windows. The budget
-- engine (internal/modules/budgets) computes used/reserved/remaining per window
-- as windowed SUM() over this table — the windows themselves are PASSED IN by
-- the caller, never read from any tier table here.
--
-- Lifecycle: Reserve -> active (counts against `reserved`); Capture -> captured
-- (counts against `used` by captured_millicents); Release -> released (frees the
-- reservation, counts against neither). Idempotent on
-- (tenant, owner, actor, source, source_id): a replayed request returns the
-- existing reservation rather than double-reserving.
--
-- RLS (issue #227): tenant-owned -> ENABLE + FORCE row level security with the
-- exact migration-050 tenant_isolation policy form + DML grants for openrails_app.
-- =============================================================================

SET lock_timeout      = '10s';
SET statement_timeout = '300s';

CREATE TABLE IF NOT EXISTS billing.budget_reservations (
    id                   UUID        PRIMARY KEY DEFAULT uuidv7(),

    -- Tenant scoping (issue #223 / #227). NOT NULL: every reservation belongs to a tenant.
    tenant_id            UUID        NOT NULL,

    -- Owner org that the budget is charged against (issue #221, the payer).
    owner_id             UUID        NOT NULL,
    -- Delegated actor (free-form subject) whose spend the windows cap.
    actor                TEXT        NOT NULL,

    -- Reserved (authorized) amount, in millicents. Counts against `reserved`
    -- while status='active'.
    amount_millicents    BIGINT      NOT NULL,
    -- Actually captured amount, in millicents. Counts against `used` once
    -- status='captured'. 0 until captured.
    captured_millicents  BIGINT      NOT NULL DEFAULT 0,

    -- Lifecycle state.
    status               TEXT        NOT NULL DEFAULT 'active'
        CHECK (status IN ('active', 'captured', 'released')),

    -- Idempotency coordinates. source_id is typically the request id.
    source               TEXT        NOT NULL,
    source_id            TEXT        NOT NULL,

    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at           TIMESTAMPTZ
);

COMMENT ON TABLE billing.budget_reservations IS
    'Rolling-window money-budget reservations (issue #304). One row per in-flight/settled charge against an actor''s passed-in windows; used/reserved/remaining are windowed SUM() over created_at. Idempotent on (tenant, owner, actor, source, source_id).';

-- Idempotency: a replayed Reserve for the same request returns the existing row
-- rather than creating a duplicate reservation.
CREATE UNIQUE INDEX uq_budget_reservations_idem
    ON billing.budget_reservations (tenant_id, owner_id, actor, source, source_id);

-- Rolling-window aggregation: scan an actor's reservations within a window by
-- created_at.
CREATE INDEX ix_budget_reservations_window
    ON billing.budget_reservations (tenant_id, owner_id, actor, created_at);

-- ---------------------------------------------------------------------------
-- RLS (issue #227): exact migration-050 tenant_isolation policy form.
-- ---------------------------------------------------------------------------
ALTER TABLE billing.budget_reservations ENABLE ROW LEVEL SECURITY;
ALTER TABLE billing.budget_reservations FORCE  ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation ON billing.budget_reservations;
CREATE POLICY tenant_isolation ON billing.budget_reservations
    USING      (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

GRANT SELECT, INSERT, UPDATE, DELETE ON billing.budget_reservations TO openrails_app;
