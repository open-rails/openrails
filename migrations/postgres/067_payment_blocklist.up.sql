-- =============================================================================
-- 067 — Tenant-scoped payment blocklist (issue #300, payment-fraud-velocity-controls)
--
-- A list of known-bad payment identifiers (card fingerprints, processor customer
-- ids, emails, IPs) so checkout/admission can later DENY them. Each row is either
-- tenant-wide (owner_id NULL) or scoped to a single tenant subject (owner_id set).
--
-- This slice builds ONLY the blocklist primitive (table + lookup). Wiring it into
-- the checkout/admission deny path — plus velocity caps and new-account default
-- tiers — is intentionally out of scope (documented hook only).
--
-- RLS (issue #227): tenant-owned -> ENABLE + FORCE row level security with the
-- exact migration-050 tenant_isolation policy form + DML grants for openrails_app.
-- =============================================================================

SET lock_timeout      = '10s';
SET statement_timeout = '300s';

CREATE TABLE IF NOT EXISTS billing.payment_blocklist (
    id          UUID        PRIMARY KEY DEFAULT uuidv7(),

    -- Tenant scoping (issue #223 / #227). NOT NULL: every entry belongs to a tenant.
    tenant_id   UUID        NOT NULL,

    -- Tenant subject the block is scoped to. NULL = tenant-wide block (applies to all
    -- owners in the tenant); set = block only for that tenant subject (issue #221).
    owner_id    UUID,

    -- What kind of identifier is blocked.
    kind        TEXT        NOT NULL CHECK (kind IN ('card_fingerprint', 'processor_customer', 'email', 'ip')),

    -- The blocked identifier value (e.g. the card fingerprint / customer id / email / ip).
    value       TEXT        NOT NULL,

    -- Optional free-form reason / note for the block (audit).
    reason      TEXT,

    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

COMMENT ON TABLE billing.payment_blocklist IS
    'Tenant-scoped blocklist of known-bad payment identifiers (issue #300). owner_id NULL = tenant-wide block; set = owner-scoped. Checkout/admission deny wiring is a separate slice.';

-- Uniqueness + the lookup index: a (tenant, kind, value) triple is unique. The
-- unique index also serves as the IsBlocked lookup (tenant_id, kind, value).
CREATE UNIQUE INDEX IF NOT EXISTS uq_payment_blocklist
    ON billing.payment_blocklist (tenant_id, kind, value);

-- ---------------------------------------------------------------------------
-- RLS (issue #227): exact migration-050 tenant_isolation policy form.
-- ---------------------------------------------------------------------------
ALTER TABLE billing.payment_blocklist ENABLE ROW LEVEL SECURITY;
ALTER TABLE billing.payment_blocklist FORCE  ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation ON billing.payment_blocklist;
CREATE POLICY tenant_isolation ON billing.payment_blocklist
    USING      (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

GRANT SELECT, INSERT, UPDATE, DELETE ON billing.payment_blocklist TO openrails_app;
