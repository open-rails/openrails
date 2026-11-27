-- The verifier reads overdue NMI renewals: the rebill watch's probe backoff
-- and the index of its scan go.

ALTER TABLE billing.rebill_cycles
    DROP CONSTRAINT rebill_cycles_probe_check,
    DROP COLUMN probe_failed_at,
    DROP COLUMN next_probe_at;

DROP INDEX billing.subscriptions_current_period_ends_at_rebill_watch_idx;
