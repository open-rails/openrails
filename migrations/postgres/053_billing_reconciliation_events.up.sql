-- =============================================================================
-- 053 — Billing reconciliation events (issue #243)
--
-- Adds the alert-first surface for the billing reconciliation + orphan-hold
-- cleanup loop. Mirrors the alert-only billing.catalog_drift_events table
-- (issue #209): the reconciliation worker records every divergence it finds
-- here BEFORE (and independently of) any safe auto-remediation, so operators
-- always see what was detected even when nothing was repaired.
--
-- The reconciliation worker performs three OpenRails-internal checks plus one
-- cross-system check:
--
--   (1) ORPHAN HOLDS — credit_transactions rows with transaction_type='hold'
--       stuck in status='active' past their expires_at (a worker died without
--       capture/release and the 5-minute HoldExpiryWorker has not yet caught
--       up, or expires_at was never set). Recorded as kind='orphan_hold', then
--       safely released (status->'expired', held_balance restored) under the
--       same row-locking discipline as the HoldExpiryWorker.
--
--   (2) HELD_BALANCE DRIFT — per (tenant, owner, credit_type) the denormalized
--       user_credit_balances.held_balance disagrees with SUM(authorized_amount)
--       over the still-active holds. Recorded as kind='held_balance_drift',
--       then corrected to the ledger-derived value.
--
--   (3) BALANCE DRIFT — per (tenant, owner, credit_type) the denormalized
--       user_credit_balances.balance disagrees with the credit_transactions
--       ledger (SUM(amount) of posted rows). Recorded as kind='balance_drift'.
--       ALERT-ONLY: the available-balance ledger has more inputs (FIFO blocks,
--       expiry) than this job models, so it reports for an operator rather than
--       auto-correcting.
--
--   (4) MISSING SETTLEMENT — a cross-system check: the host feeds a set of
--       expected settlements (one per usage/billing event emitted by Tensorhub)
--       and the job diffs them against the credit_transactions ledger, flagging
--       expected settlements that have no matching capture/withdrawal (a held-
--       but-never-settled request or a lost settle call) and, symmetrically,
--       captures with no expected settlement (double-charge candidates). The
--       Tensorhub feed itself is the HOST's responsibility (Tensorhub data is
--       not in this repo); OpenRails exposes the diff INTERFACE + report surface.
--
-- ALERT-FIRST / additive: this is a new table only; no existing rows change and
-- no backfill is needed. Rows are owner/tenant-scoped (issue #221/#223). Open
-- events dedupe on (tenant, owner, credit_type, kind, subject_id) so reruns are
-- idempotent — a divergence that is still present is not re-inserted, and a
-- divergence that is gone is auto-resolved (resolved_at stamped).
-- =============================================================================

SET lock_timeout      = '10s';
SET statement_timeout = '300s';

CREATE TABLE IF NOT EXISTS billing.reconciliation_events (
    id              UUID         PRIMARY KEY DEFAULT uuidv7(),

    -- Owner/tenant scope (issue #221/#223). NULL-able only for cross-system
    -- orphans that carry no owner context; the internal checks always set them.
    tenant_id       UUID,
    owner_id        UUID,
    credit_type_id  UUID,

    kind            TEXT         NOT NULL CHECK (kind IN (
                        'orphan_hold',
                        'held_balance_drift',
                        'balance_drift',
                        'missing_settlement',
                        'unexpected_capture'
                    )),

    -- subject_id identifies the concrete subject of the event so open rows
    -- dedupe stably: the hold transaction id for orphan_hold, the balance row
    -- id for *_drift, the expected-settlement key (source_id) for
    -- missing_settlement, and the capture transaction id for unexpected_capture.
    subject_id      TEXT,

    -- Stringified expected vs. observed values for drift/diff kinds (e.g.
    -- denormalized held_balance vs. ledger-derived sum). Empty for orphan_hold.
    expected_value  TEXT,
    observed_value  TEXT,

    -- remediated_at is stamped when the worker SAFELY auto-repaired the
    -- divergence (released an orphan hold, corrected held_balance). NULL means
    -- alert-only (balance_drift, missing_settlement) or not-yet-remediated.
    remediated_at   TIMESTAMPTZ,

    detected_at     TIMESTAMPTZ  NOT NULL DEFAULT current_timestamp,
    resolved_at     TIMESTAMPTZ
);

-- Cheap scan of currently-open events (the alert surface).
CREATE INDEX IF NOT EXISTS idx_reconciliation_events_open
    ON billing.reconciliation_events (detected_at DESC) WHERE resolved_at IS NULL;

-- Owner-scoped lookups for the report surface.
CREATE INDEX IF NOT EXISTS idx_reconciliation_events_owner
    ON billing.reconciliation_events (tenant_id, owner_id, credit_type_id)
    WHERE resolved_at IS NULL;

-- Open events are unique per (scope, kind, subject) so reruns are idempotent.
CREATE UNIQUE INDEX IF NOT EXISTS uq_reconciliation_events_open
    ON billing.reconciliation_events (
        COALESCE(tenant_id, '00000000-0000-0000-0000-000000000000'::uuid),
        COALESCE(owner_id, '00000000-0000-0000-0000-000000000000'::uuid),
        COALESCE(credit_type_id, '00000000-0000-0000-0000-000000000000'::uuid),
        kind,
        COALESCE(subject_id, '')
    ) WHERE resolved_at IS NULL;

COMMENT ON TABLE billing.reconciliation_events IS
    'Alert-first divergence records from the billing reconciliation + orphan-hold cleanup loop (issue #243): orphan holds, held_balance drift, ledger-vs-denormalized balance drift, and cross-system (Tensorhub usage vs OpenRails ledger) settlement diffs. Recorded before any safe auto-remediation; open rows dedupe on (tenant, owner, credit_type, kind, subject_id).';
