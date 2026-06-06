-- =============================================================================
-- 064 — Append-only usage events (issue #289)
--
-- The durable, multi-dimensional record of metered API usage. One row per
-- metered request: the host (Tensorhub/gen-orch) PRICES the event and sends the
-- amount; OpenRails records the event AND debits the credit ledger in the SAME
-- transaction (see internal/modules/credits.RecordUsage), so the event and the
-- balance change commit together (never double-count, never lose an event).
--
-- This is the SOURCE OF TRUTH for usage reporting + the #303 monthly invoice
-- line items. Aggregation (rollups) is windowed SQL over this table. The hot
-- admission path (#298) NEVER reads/aggregates this table — it uses Redis.
--
-- RLS (issue #227): tenant-owned -> ENABLE + FORCE row level security with the
-- exact migration-050 tenant_isolation policy form + DML grants for openrails_app.
-- =============================================================================

SET lock_timeout      = '10s';
SET statement_timeout = '300s';

CREATE TABLE IF NOT EXISTS billing.usage_events (
    id              UUID        PRIMARY KEY DEFAULT uuidv7(),

    -- Tenant scoping (issue #223 / #227). NOT NULL: every event belongs to a tenant.
    tenant_id       UUID        NOT NULL,

    -- Tenant subject that is BILLED for this usage (issue #221, the payer).
    owner_id        UUID        NOT NULL,
    -- Actor that caused the usage (attribution only; not the financial owner).
    user_id         TEXT        NOT NULL,

    -- The credit type debited for this event.
    credit_type_id  UUID        NOT NULL,

    -- The metered endpoint / model (e.g. "gpt-4o"); groups invoice line items.
    event_type      TEXT        NOT NULL,

    -- Per-dimension counts: {input_tokens, output_tokens, cached_input_tokens,
    -- requests, images, gpu_seconds, ...}. Host-defined, used for reporting +
    -- the #298 throughput counters. Default '{}' so aggregation is uniform.
    dimensions      JSONB       NOT NULL DEFAULT '{}',

    -- Host-priced cost in the credit type's smallest unit (e.g. millicents).
    -- OpenRails does NOT compute price; the host sends it. >= 0.
    amount          BIGINT      NOT NULL CHECK (amount >= 0),

    -- Idempotency coordinates. source_id is typically the request id.
    source          TEXT        NOT NULL,
    source_id       TEXT        NOT NULL,

    -- Link back to the ledger debit this event produced (audit / reconciliation).
    credit_transaction_id UUID,

    metadata        JSONB,

    occurred_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

COMMENT ON TABLE billing.usage_events IS
    'Append-only multi-dimensional metered usage (issue #289). Source of truth for usage reporting + #303 invoice line items. Host-priced (amount sent by the host); event + ledger debit commit in one tx. The hot admission path (#298) never reads this table.';

-- Idempotency: re-processing the same metered request must NOT create a duplicate
-- event nor a duplicate debit. (tenant, owner, event_type, source, source_id).
CREATE UNIQUE INDEX IF NOT EXISTS uq_usage_events_idem
    ON billing.usage_events (tenant_id, owner_id, event_type, source, source_id);

-- Windowed aggregation for rollups / invoices: scan an owner's events in a period,
-- optionally narrowed by event_type.
CREATE INDEX IF NOT EXISTS ix_usage_events_owner_time
    ON billing.usage_events (tenant_id, owner_id, occurred_at);
CREATE INDEX IF NOT EXISTS ix_usage_events_owner_type_time
    ON billing.usage_events (tenant_id, owner_id, event_type, occurred_at);

-- ---------------------------------------------------------------------------
-- RLS (issue #227): exact migration-050 tenant_isolation policy form.
-- ---------------------------------------------------------------------------
ALTER TABLE billing.usage_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE billing.usage_events FORCE  ROW LEVEL SECURITY;

DROP POLICY IF EXISTS tenant_isolation ON billing.usage_events;
CREATE POLICY tenant_isolation ON billing.usage_events
    USING      (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = nullif(current_setting('app.tenant_id', true), '')::uuid);

GRANT SELECT, INSERT, UPDATE, DELETE ON billing.usage_events TO openrails_app;
