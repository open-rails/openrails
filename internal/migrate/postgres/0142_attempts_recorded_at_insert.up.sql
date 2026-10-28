-- Repair: none-needed. An attempt records its card and answer when it is
-- inserted; no later pass reads the PSP's transaction report to fill it.
DROP INDEX billing.payment_attempts_attempted_at_merchant_id_unenriched_idx;
DROP INDEX billing.payment_attempts_attempted_at_unenriched_idx;
ALTER TABLE billing.payment_attempts DROP COLUMN enriched_at;
