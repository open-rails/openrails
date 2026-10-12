-- parent: 72 sha256:de38170fa42bc1f56d0f59653635d57077ab1c71b267dcc348d554e6eaafb854
-- Repair: none-needed Stated amounts are optional for historical rows. The failed-event check applies to new and updated rows only; historical rows are not validated.
ALTER TABLE billing.usage_events
    DROP CONSTRAINT usage_events_failed_host_priced_check,
    DROP COLUMN pricing_authority,
    ADD COLUMN stated_amount bigint CHECK (stated_amount >= 0);
ALTER TABLE billing.usage_events
    ADD CONSTRAINT usage_events_failed_stated_amount_check
    CHECK (outcome = 'succeeded' OR stated_amount IS NOT NULL) NOT VALID;
COMMENT ON COLUMN billing.usage_events.stated_amount IS 'Optional host cost in native currency units. Dynamic meter cards charge it; other cards ignore it. Failed events must state their cost.';
COMMENT ON COLUMN billing.usage_events.charges IS 'What each meter charged the event: [{meter_key, model, cell, quantity, amount}]. Empty when no meter consumes it or the usage failed.';
COMMENT ON COLUMN billing.admission_operations.estimate_usage IS 'The estimated usage ({event_type, dimensions, metadata, amount}); dynamic cards charge its optional amount. estimated_amount is its price when admitted. Null for a host-stated hold.';
