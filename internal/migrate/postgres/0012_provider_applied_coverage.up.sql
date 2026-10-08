-- parent: 11 sha256:c5e2703d3c5bd79f7e3f6a1b5d73633540c89c3fadd1e6a3886a37f4e4e7f03f
-- Repair: preserve Existing observation cursors are never promoted to applied coverage.
-- Observation cursors from earlier versions (including advisory-only pulls)
-- cannot attest that the local financial mirror caught up. Keep them intact;
-- applied_events is recorded only by complete, successfully applied windows.
ALTER TABLE billing.psp_refresh_watermarks
    DROP CONSTRAINT psp_refresh_watermarks_event_domain_check;
ALTER TABLE billing.psp_refresh_watermarks
    ADD CONSTRAINT psp_refresh_watermarks_event_domain_check
    CHECK (event_domain IN ('events', 'applied_events', 'completed_events'));
COMMENT ON COLUMN billing.psp_refresh_watermarks.event_domain IS
    'events is observed provider-window progress; applied_events is complete provider-window coverage with required financial receipts recovered; completed_events is a fully caught-up captured target. None is provider finality or a cross-database clone fence.';
