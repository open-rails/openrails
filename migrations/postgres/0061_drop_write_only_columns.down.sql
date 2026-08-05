-- Restore the write-only columns in their 0001 shape (types, nullability,
-- defaults, comments). Values are NOT recoverable and none ever mattered: every
-- column here was written and never read, so re-adding the defaults restores
-- exactly the information content the deployment had.

SET LOCAL statement_timeout = '60s';
SET LOCAL lock_timeout = '10s';

ALTER TABLE openrails.invoker_spend_limits
    ADD COLUMN policy_version bigint DEFAULT 1 NOT NULL;

ALTER TABLE openrails.merchant_configurations
    ADD COLUMN config_version bigint DEFAULT 1 NOT NULL;

ALTER TABLE openrails.tier_schedules
    ADD COLUMN schedule_version bigint DEFAULT 1 NOT NULL;

ALTER TABLE openrails.product_usage_limit_bindings
    ADD COLUMN source_type text NOT NULL DEFAULT '',
    ADD COLUMN source_id uuid,
    ADD COLUMN product_key text,
    ADD COLUMN policy_version bigint DEFAULT 1 NOT NULL;

-- 0001 declared source_type NOT NULL with no default; the default above exists
-- only so the ADD succeeds against existing rows. Drop it so the restored shape
-- matches (new writers must supply the value, as they did before).
ALTER TABLE openrails.product_usage_limit_bindings
    ALTER COLUMN source_type DROP DEFAULT;

COMMENT ON COLUMN openrails.product_usage_limit_bindings.product_key IS
    'Catalog product key/slug whose current benefits were materialized at grant time.';

ALTER TABLE openrails.webhook_health
    ADD COLUMN accepted_count bigint DEFAULT 0 NOT NULL,
    ADD COLUMN last_rejected_at timestamp with time zone,
    ADD COLUMN rejected_count bigint DEFAULT 0 NOT NULL,
    ADD COLUMN last_drift_at timestamp with time zone,
    ADD COLUMN drift_count bigint DEFAULT 0 NOT NULL;

COMMENT ON COLUMN openrails.webhook_health.drift_count IS
    'pull-derived corrections applied while last_accepted_at predated the previous pull — changes a webhook should have announced.';

ALTER TABLE openrails.webhook_health_daily
    ADD COLUMN accepted bigint DEFAULT 0 NOT NULL;

ALTER TABLE openrails.rail_refresh_watermarks
    ADD COLUMN last_attempted_at timestamp with time zone,
    ADD COLUMN last_succeeded_at timestamp with time zone,
    ADD COLUMN last_error text;

COMMENT ON TABLE openrails.rail_refresh_watermarks IS
    'Durable Provider Refresh watermarks. A failed or partial provider read records last_error but never advances watermark_at.';

ALTER TABLE openrails.usage_events
    ADD COLUMN invoker_type text;

ALTER TABLE openrails.merchant_deks
    ADD COLUMN key_version integer DEFAULT 1 NOT NULL;

ALTER TABLE openrails.money_settings
    ADD COLUMN last_alert_at timestamp with time zone;

ALTER TABLE openrails.alert_rules
    ADD COLUMN last_detail jsonb;

ALTER TABLE openrails.reconciliation_state
    ADD COLUMN last_full_pull_at timestamp with time zone;
