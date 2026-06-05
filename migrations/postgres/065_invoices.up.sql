-- =============================================================================
-- 065 — Monthly itemized invoices / statements (issue #303)
--
-- A per-period (monthly) human-readable STATEMENT for every customer (prepaid AND
-- arrears): what they were billed for, for their own accounting. Built from the
-- #289 usage_events log (line items) + the credit ledger (money movements). NOT
-- Lago's fee-taxonomy/charge-model engine — line items are a rollup of the data
-- we already record. OpenRails still owns the charge; for arrears the invoice is
-- the statement the #301 sweep settles, for prepaid it is an informational receipt.
--
-- Line items + money movements are snapshotted (JSONB) at finalize for
-- immutability. Scoped per (owner, credit_type, period).
--
-- RLS (issue #227): exact migration-050 tenant_isolation policy form.
-- =============================================================================

SET lock_timeout      = '10s';
SET statement_timeout = '300s';

CREATE TABLE IF NOT EXISTS billing.invoices (
    id              UUID        PRIMARY KEY DEFAULT uuidv7(),

    tenant_id       UUID        NOT NULL,
    owner_id        UUID        NOT NULL,
    credit_type_id  UUID        NOT NULL,
    -- Display unit of the credit type (e.g. "cents").
    currency        TEXT        NOT NULL DEFAULT '',

    period_from     TIMESTAMPTZ NOT NULL,
    period_to       TIMESTAMPTZ NOT NULL,

    -- Totals in the credit type's smallest unit.
    usage_total     BIGINT      NOT NULL DEFAULT 0,
    deposits_total  BIGINT      NOT NULL DEFAULT 0,
    owed_accrued    BIGINT      NOT NULL DEFAULT 0,
    owed_paid       BIGINT      NOT NULL DEFAULT 0,
    -- Balance at finalize time (closing snapshot).
    closing_balance BIGINT      NOT NULL DEFAULT 0,

    -- Immutable snapshots taken at finalize.
    line_items      JSONB       NOT NULL DEFAULT '[]',
    money_movements JSONB       NOT NULL DEFAULT '{}',

    status          TEXT        NOT NULL DEFAULT 'draft' CHECK (status IN ('draft', 'finalized', 'voided')),
    finalized_at    TIMESTAMPTZ,

    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

COMMENT ON TABLE billing.invoices IS
    'Monthly itemized statements (issue #303). Line items rolled up from billing.usage_events; money movements from the credit ledger; snapshotted at finalize. Prepaid = receipt, arrears = statement the #301 sweep settles.';

-- Idempotency: one invoice per (owner, credit_type, period).
CREATE UNIQUE INDEX IF NOT EXISTS uq_invoices_period
    ON billing.invoices (tenant_id, owner_id, credit_type_id, period_from, period_to);

CREATE INDEX IF NOT EXISTS ix_invoices_owner
    ON billing.invoices (tenant_id, owner_id, period_from DESC);

ALTER TABLE billing.invoices ENABLE ROW LEVEL SECURITY;
ALTER TABLE billing.invoices FORCE  ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation ON billing.invoices;
CREATE POLICY tenant_isolation ON billing.invoices
    USING      (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

GRANT SELECT, INSERT, UPDATE, DELETE ON billing.invoices TO openrails_app;
