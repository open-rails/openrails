-- parent: 7 sha256:0a6ac0886ba4b019a0187612d2549b4b03d8077a86e4818b694cbe253ff72158
-- Repair: preserve Existing observation cursors are never promoted to applied coverage.
-- Observation cursors from earlier versions (including advisory-only pulls)
-- cannot attest that the local financial mirror caught up. Keep them intact;
-- applied_events is recorded only by complete, successfully applied windows.
ALTER TABLE billing.psp_refresh_watermarks
    DROP CONSTRAINT psp_refresh_watermarks_event_domain_check;
ALTER TABLE billing.psp_refresh_watermarks
    ADD CONSTRAINT psp_refresh_watermarks_event_domain_check
    CHECK (event_domain IN ('events', 'applied_events'));
COMMENT ON COLUMN billing.psp_refresh_watermarks.event_domain IS
    'events is observed provider-window progress; applied_events is complete provider-window coverage with required financial receipts recovered. Neither is provider finality or a cross-database clone fence.';
