-- parent: 36 sha256:fa71ba06404d54ecd34126b8c1f5efd05095e746241060e9469b5b1f1fc5470c
-- A cycle NMI attempted that no attempt records is a miss OpenRails never
-- collects: provider_reversed (its sale was voided or refunded in full) or
-- provider_unrecorded (any other transaction NMI holds for the cycle).
SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '5min';

ALTER TABLE openrails.rebill_cycles DROP CONSTRAINT chk_rebill_cycles_miss_reason;
ALTER TABLE openrails.rebill_cycles
    ADD CONSTRAINT chk_rebill_cycles_miss_reason CHECK (miss_reason IN ('held', 'refused', 'method_unusable', 'not_attempted', 'provider_skipped', 'provider_stalled', 'provider_reversed', 'provider_unrecorded', 'schedule_gone')) NOT VALID;
