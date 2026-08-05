-- Restore the retired #599 meter kind column. Values are not recoverable: the
-- shape that produced them no longer exists in the manifest.

SET LOCAL statement_timeout = '60s';
SET LOCAL lock_timeout = '10s';

ALTER TABLE openrails.catalog_meters
    ADD COLUMN kind text;

ALTER TABLE openrails.catalog_meters
    ADD CONSTRAINT catalog_meters_kind_check
    CHECK ((kind IS NULL) OR (kind = ANY (ARRAY['counter'::text, 'gauge'::text])));

COMMENT ON COLUMN openrails.catalog_meters.kind IS 'counter = summed events; gauge = time-integrated level samples.';
