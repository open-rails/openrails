-- The subscription's lifecycle revision right after a destructive run's own
-- write. A reverse restores the row only while it is unchanged since, so it
-- never rewrites a renewal, cancel or payment that came after the run.
ALTER TABLE billing.destructive_run_before_images ADD COLUMN after_lifecycle_rev bigint;
COMMENT ON COLUMN billing.destructive_run_before_images.after_lifecycle_rev IS 'The subscription''s lifecycle_rev right after the run''s write. The reverse restores the row only while lifecycle_rev still equals it; NULL (an access image, or a write the run never sealed) is never restored.';
