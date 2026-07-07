-- #786 webhook-health detection: silence watermark + signature-reject counter +
-- pull-drift signal, surfaced as #736 alerts over #733 metrics.
--
-- webhook_health       — one row per (merchant, rail): verified-accepted /
--                        rejected / drift watermarks + lifetime counters, plus
--                        the provider-refresh pull watermark the drift gate
--                        compares against. Cheap UPSERT per event.
-- webhook_health_daily — UTC-day counter buckets so the #733 metrics engine can
--                        window reject/drift rates (same shape as
--                        admission_denials_hourly; alert windows are >= 1d).

CREATE TABLE openrails.webhook_health (
    merchant_id uuid NOT NULL,
    rail text NOT NULL,
    -- Stamped ONLY by verified-accepted webhooks; rejected deliveries never
    -- touch it (the whole point of the silence signal).
    last_accepted_at timestamp with time zone,
    accepted_count bigint NOT NULL DEFAULT 0,
    last_rejected_at timestamp with time zone,
    rejected_count bigint NOT NULL DEFAULT 0,
    last_drift_at timestamp with time zone,
    drift_count bigint NOT NULL DEFAULT 0,
    -- Provider-refresh pull watermark: during a refresh this still holds the
    -- PREVIOUS pull's time (stamped after the pass), which is what the drift
    -- gate compares last_accepted_at against.
    last_pull_at timestamp with time zone,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT webhook_health_rail_nonempty CHECK ((btrim(rail) <> ''::text))
);

COMMENT ON TABLE openrails.webhook_health IS '#786 per-(merchant, rail) inbound-webhook health: accepted/rejected/drift watermarks + counters. last_accepted_at is stamped only by signature-verified webhooks; last_pull_at is the provider-refresh pull watermark the drift gate uses.';
COMMENT ON COLUMN openrails.webhook_health.last_accepted_at IS 'last signature-VERIFIED webhook for this rail; silence age is measured from here (or created_at when nothing was ever accepted).';
COMMENT ON COLUMN openrails.webhook_health.drift_count IS 'pull-derived corrections applied while last_accepted_at predated the previous pull — changes a webhook should have announced.';

ALTER TABLE ONLY openrails.webhook_health
    ADD CONSTRAINT webhook_health_pkey PRIMARY KEY (merchant_id, rail);
ALTER TABLE ONLY openrails.webhook_health
    ADD CONSTRAINT webhook_health_merchant_fk FOREIGN KEY (merchant_id) REFERENCES openrails.merchants(id) ON DELETE CASCADE;

ALTER TABLE openrails.webhook_health ENABLE ROW LEVEL SECURITY;
ALTER TABLE ONLY openrails.webhook_health FORCE ROW LEVEL SECURITY;
CREATE POLICY merchant_isolation ON openrails.webhook_health USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));
GRANT SELECT,INSERT,UPDATE,DELETE ON TABLE openrails.webhook_health TO openrails_app;

CREATE TABLE openrails.webhook_health_daily (
    merchant_id uuid NOT NULL,
    rail text NOT NULL,
    day_at timestamp with time zone NOT NULL,
    accepted bigint NOT NULL DEFAULT 0,
    rejected bigint NOT NULL DEFAULT 0,
    drift bigint NOT NULL DEFAULT 0,
    CONSTRAINT webhook_health_daily_rail_nonempty CHECK ((btrim(rail) <> ''::text))
);

COMMENT ON TABLE openrails.webhook_health_daily IS '#786 UTC-day webhook counter buckets backing the #733 webhook_rejects / webhook_drift_events windowed metrics.';

ALTER TABLE ONLY openrails.webhook_health_daily
    ADD CONSTRAINT webhook_health_daily_pkey PRIMARY KEY (merchant_id, rail, day_at);
ALTER TABLE ONLY openrails.webhook_health_daily
    ADD CONSTRAINT webhook_health_daily_merchant_fk FOREIGN KEY (merchant_id) REFERENCES openrails.merchants(id) ON DELETE CASCADE;

ALTER TABLE openrails.webhook_health_daily ENABLE ROW LEVEL SECURITY;
ALTER TABLE ONLY openrails.webhook_health_daily FORCE ROW LEVEL SECURITY;
CREATE POLICY merchant_isolation ON openrails.webhook_health_daily USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));
GRANT SELECT,INSERT,UPDATE,DELETE ON TABLE openrails.webhook_health_daily TO openrails_app;
