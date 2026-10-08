-- parent: 8 sha256:08786eb308b4957bbabcbd12bca513fbd1592c4d7ddde3e6f50c938d809538dd
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
