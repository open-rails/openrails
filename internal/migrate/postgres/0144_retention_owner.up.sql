-- Repair: none-needed The cleanup job owns every row retention. Metric
-- counters are kept 13 months; the state of a kind no fleet has used for 30
-- days goes.
CREATE INDEX webhook_health_daily_day_at_idx ON billing.webhook_health_daily USING btree (merchant_id, day_at);
COMMENT ON TABLE billing.admission_denials_hourly IS 'Hourly admission-denial aggregates (merchant x payer x reason), flushed from Redis counters once each hour closes; the hot path never writes PG per-request. Retention: rows are deleted 13 months (397 days) after hour_at.';
COMMENT ON TABLE billing.webhook_health_daily IS 'UTC-day webhook counter buckets per event source, backing the webhook_rejects and webhook_drift_events windowed metrics. Retention: rows are deleted 13 months (397 days) after day_at.';
COMMENT ON TABLE billing.periodic_leases IS 'Global by design: which fleet (River schema) queues a periodic kind''s pass for the database; one row per kind, taken over once expired, deleted 30 days after it expired.';
COMMENT ON TABLE billing.worker_state IS 'Global by design: operator-global worker health and fair sweep progress. Health and cursor writers update only their own fields; every fleet''s registration touches updated_at, and a kind none has touched for 30 days is deleted. NULL cursor starts at the beginning; otherwise restart resumes after cursor_merchant_id.';
