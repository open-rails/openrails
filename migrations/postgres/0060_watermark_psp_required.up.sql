-- or#893 phase 1: rail_refresh_watermarks loses its NULL/global lane.
--
-- The column comment called NULL "the compatibility/global lane for providers
-- without a bound account identity". There is no such provider. Every pull arms
-- from exactly ONE PSP (reconcile.MerchantFetcherBuilder resolves it and records
-- it as PSPCoverage.Binding) — the refresh worker simply threw that binding away
-- and passed nil, so 100% of watermarks landed on the global lane.
--
-- That is not a compatibility detail, it is data loss: a merchant running mobius
-- and paykings on nmi shares ONE watermark row. Pulling mobius advances it past
-- a window paykings never read, and paykings' events in that window are never
-- fetched again — the watermark is an EXCLUSIVE lower bound, so a skipped window
-- is skipped permanently. It also lets one PSP's freshness stand as evidence
-- about another's subscriptions (ListLapsedSubscriptionsWithEvidence's
-- watermark_newer_than_period_end leg, tightened alongside this migration).
--
-- The psp_key generated column existed only to give the NULL lane a unique key.
-- With psp_id required, the identity is (merchant, rail, psp, domain) directly.
--
-- Prelaunch hard cut: existing global-lane rows are DELETED, not backfilled.
-- A watermark is a resumable cursor, not a record — the next pass re-derives it
-- from initial_lookback. No production database exists.

SET LOCAL statement_timeout = '60s';
SET LOCAL lock_timeout = '10s';

DELETE FROM openrails.rail_refresh_watermarks WHERE psp_id IS NULL;

ALTER TABLE openrails.rail_refresh_watermarks
    DROP CONSTRAINT rail_refresh_watermarks_identity_key;

-- psp_key existed ONLY to give the NULL lane a unique key. Dropping it is the
-- point of the cut, and it is a generated column no client selects.
ALTER TABLE openrails.rail_refresh_watermarks
    -- squawk-ignore ban-drop-column
    DROP COLUMN psp_key;

ALTER TABLE openrails.rail_refresh_watermarks
    DROP CONSTRAINT rail_refresh_watermarks_psp_nonzero;

-- The DELETE above already removed every row that could violate it, so the
-- validating scan is over a table with at most one row per (merchant, rail, PSP).
ALTER TABLE openrails.rail_refresh_watermarks
    -- squawk-ignore adding-not-nullable-field
    ALTER COLUMN psp_id SET NOT NULL;

-- Re-keying the identity: this is the SAME constraint the DROP above removed,
-- minus the psp_key indirection. Cursor table, one row per (merchant, rail, PSP,
-- domain) — the index build is over a handful of rows.
ALTER TABLE openrails.rail_refresh_watermarks
    -- squawk-ignore disallowed-unique-constraint, constraint-missing-not-valid
    ADD CONSTRAINT rail_refresh_watermarks_identity_key
    UNIQUE (merchant_id, rail, psp_id, event_domain);

-- ON DELETE SET NULL was the global lane's other door: deleting a PSP silently
-- demoted its watermark to "everyone's". A watermark has no meaning without the
-- PSP whose event stream it bounds.
ALTER TABLE openrails.rail_refresh_watermarks
    DROP CONSTRAINT rail_refresh_watermarks_psp_fk;

ALTER TABLE openrails.rail_refresh_watermarks
    -- squawk-ignore adding-foreign-key-constraint, constraint-missing-not-valid
    ADD CONSTRAINT rail_refresh_watermarks_psp_fk
    FOREIGN KEY (psp_id) REFERENCES openrails.psps(id) ON DELETE CASCADE;

DROP INDEX openrails.idx_rail_refresh_watermarks_account;

CREATE INDEX idx_rail_refresh_watermarks_psp
    ON openrails.rail_refresh_watermarks USING btree (psp_id);

COMMENT ON COLUMN openrails.rail_refresh_watermarks.psp_id IS
    'The PSP whose event stream this cursor bounds. Required (or#893): a pull arms from exactly one PSP, and a watermark shared across PSPs skips the events of every PSP but the one that advanced it.';
