-- parent: 72 sha256:6014d4a123ab414923c9d5f6b8a5e44bbd4daab9d6f256ac7c89c84de84df8c6
-- Repair: none-needed Recorded usage kept its resource in metadata alone; the backfill moves a non-blank one to the column, and the rest stay NULL.
-- A recorded usage event's resource is its resource column, which the usage
-- report and the metrics read; recording kept it in the event's metadata.
UPDATE billing.usage_events
SET resource = btrim(metadata ->> 'resource')
WHERE resource IS NULL
  AND jsonb_typeof(metadata -> 'resource') = 'string'
  AND btrim(metadata ->> 'resource') <> '';
