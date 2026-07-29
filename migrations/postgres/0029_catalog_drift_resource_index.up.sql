-- or#846 follow-up: ResolveCatalogDriftForResource discarded catalog_drift_events
-- rows on openrails_resource_id with no index covering it (sqlaudit
-- [unindexed-filter]).
--
-- uq_catalog_drift_open exists but leads with (merchant_id, rail, kind) and this
-- query constrains neither rail nor kind, so it degenerates to a scan of the
-- merchant's open drift rows. Index the columns the query actually filters on,
-- in that order: merchant_id (the RLS predicate is always present), then the
-- resource identity. Partial on the same `resolved_at IS NULL` the query and the
-- other open-drift indexes use — resolved rows are never looked up this way and
-- are the bulk of the table over time.
SET statement_timeout = '60s';
SET lock_timeout = '10s';

CREATE INDEX IF NOT EXISTS idx_catalog_drift_open_resource
    ON openrails.catalog_drift_events
    USING btree (merchant_id, openrails_resource_type, openrails_resource_id)
    WHERE resolved_at IS NULL;
