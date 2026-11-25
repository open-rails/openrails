-- NMI history reads are due work. Every current NMI PSP has a row, queued
-- when its configuration declares it, and the hourly openrails.nmi_history job
-- reads only the rows whose due_at has come, by an index: a completed read is
-- due again a day later, a failed one stays due. A PSP that is archived,
-- superseded or whose merchant is deleted is parked at infinity until its
-- configuration declares it again.
ALTER TABLE billing.nmi_history_reads ALTER COLUMN read_at DROP NOT NULL;
ALTER TABLE billing.nmi_history_reads ADD COLUMN due_at timestamptz;
UPDATE billing.nmi_history_reads SET due_at = read_at + interval '23 hours';
INSERT INTO billing.nmi_history_reads (merchant_id, psp_id, due_at)
SELECT p.merchant_id, p.id, now()
FROM billing.psps p
JOIN billing.merchants m ON m.id = p.merchant_id
WHERE p.rail = 'nmi' AND p.superseded_at IS NULL AND m.deleted_at IS NULL
ON CONFLICT (merchant_id, psp_id) DO NOTHING;
ALTER TABLE billing.nmi_history_reads ALTER COLUMN due_at SET NOT NULL;

CREATE INDEX nmi_history_reads_due_at_idx ON billing.nmi_history_reads (due_at, merchant_id, psp_id);

COMMENT ON TABLE billing.nmi_history_reads IS 'Each current NMI PSP''s history read: when it last completed and when the next is due. A PSP never read is backfilled 25 months; later reads start the month before the last.';
COMMENT ON COLUMN billing.nmi_history_reads.read_at IS 'When the last read completed; NULL until one has.';
COMMENT ON COLUMN billing.nmi_history_reads.due_at IS 'When the hourly openrails.nmi_history job reads it next: a day after a completed read, unchanged after a failed one; infinity while the PSP is archived, superseded or its merchant deleted, until its configuration declares it again.';
