-- or#893 phase 5: one catalog pricing input. Drop openrails.catalog_meters.kind.
--
-- `kind: counter|gauge` was the #599 meter shape, paired with the `metered:`
-- price sugar. #707 deleted the second runtime metered engine but kept the
-- INPUT translation; or#893 deletes the input. The manifest no longer has a
-- Meter.Kind field, the applier no longer writes the column, and the rating
-- join no longer reads it — the `missing_default` CASE that turned a
-- kind='counter' meter into "each event counts as 1" was the last reader, and
-- its canonical replacement is `aggregation: count`.
--
-- The column is therefore dead as of this migration's own change set, not
-- merely unused. Prelaunch: no dual-read, no backfill.
--
-- What STAYS: `aggregation` (the one meter shape), and every other
-- catalog_meters column. Rate cards keep their FK onto (merchant_id, key).

SET LOCAL statement_timeout = '60s';
SET LOCAL lock_timeout = '10s';

ALTER TABLE openrails.catalog_meters
    DROP CONSTRAINT catalog_meters_kind_check;

ALTER TABLE openrails.catalog_meters
    -- squawk-ignore ban-drop-column
    DROP COLUMN kind;
