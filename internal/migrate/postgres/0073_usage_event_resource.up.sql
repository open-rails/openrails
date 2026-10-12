-- parent: 72 sha256:de38170fa42bc1f56d0f59653635d57077ab1c71b267dcc348d554e6eaafb854
-- Repair: none-needed Recorded usage kept its resource in metadata alone; the backfill moves a non-blank one to the column, and the rest stay NULL.
-- A recorded usage event's resource is its resource column, which the usage
-- report and the metrics read; recording kept it in the event's metadata.
UPDATE billing.usage_events
SET resource = btrim(metadata ->> 'resource')
WHERE resource IS NULL
  AND jsonb_typeof(metadata -> 'resource') = 'string'
  AND btrim(metadata ->> 'resource') <> '';
