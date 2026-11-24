-- How far a usage posting read the event store: the latest ingestion among
-- the usage it took. A later posting takes, of what occurred before the
-- earlier cut and was ingested by then, no less than what posted: usage the
-- store no longer holds (a restore, another store attached, a purge) is never
-- read as a price that fell, and usage ingested since is priced on top.
ALTER TABLE billing.usage_postings ADD COLUMN ingested_through timestamptz;
UPDATE billing.usage_postings SET ingested_through = created_at;
ALTER TABLE billing.usage_postings ALTER COLUMN ingested_through SET NOT NULL;
COMMENT ON COLUMN billing.usage_postings.ingested_through IS 'The latest ingestion among the usage the posting read before its cut, or an earlier posting''s when later: a later posting counts usage that occurred before cut_at and was ingested by then at no less than what posted.';
