-- Repair: none-needed New nullable columns.
-- A failed look at an overdue rebill waits out a backoff before the next.
ALTER TABLE billing.rebill_cycles
    ADD COLUMN probe_failed_at timestamptz,
    ADD COLUMN next_probe_at timestamptz,
    ADD CONSTRAINT rebill_cycles_probe_check CHECK ((probe_failed_at IS NULL) = (next_probe_at IS NULL));
COMMENT ON COLUMN billing.rebill_cycles.probe_failed_at IS 'First failed look at this overdue cycle since the last success; NULL when none failed.';
COMMENT ON COLUMN billing.rebill_cycles.next_probe_at IS 'When the rebill watch looks at this overdue cycle again after a failed look.';
