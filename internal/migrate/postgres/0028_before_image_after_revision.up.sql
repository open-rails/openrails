-- parent: 27 sha256:3b6e1f6248ca0b5c187509980b8c2c9d84c2a56f810b2811a4e6e082aa857d9b
-- The subscription's lifecycle revision right after a destructive run's own
-- write. A reverse restores the row only while it is unchanged since, so it
-- never rewrites a renewal, cancel or payment that came after the run.
ALTER TABLE billing.destructive_run_before_images ADD COLUMN after_lifecycle_rev bigint;
COMMENT ON COLUMN billing.destructive_run_before_images.after_lifecycle_rev IS 'The subscription''s lifecycle_rev right after the run''s write. The reverse restores the row only while lifecycle_rev still equals it; NULL (an access image, or a write the run never sealed) is never restored.';
