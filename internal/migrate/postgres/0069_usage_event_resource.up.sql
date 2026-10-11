-- parent: 68 sha256:bdbd000a55eb1f32c062eed4665e2a82d2fd6f26b00894e40209977256b60693
-- Repair: none-needed Recorded usage kept its resource in metadata alone; the backfill moves a non-blank one to the column, and the rest stay NULL.
-- A recorded usage event's resource is its resource column, which the usage
-- report and the metrics read; recording kept it in the event's metadata.
UPDATE billing.usage_events
SET resource = btrim(metadata ->> 'resource')
WHERE resource IS NULL
  AND jsonb_typeof(metadata -> 'resource') = 'string'
  AND btrim(metadata ->> 'resource') <> '';
