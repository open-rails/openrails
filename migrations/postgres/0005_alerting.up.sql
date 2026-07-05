-- #736 metric threshold alerting: push warnings off the #733 metrics registry.
-- Three merchant-scoped tables, all RLS + FORCE RLS + merchant_isolation
-- (matching sibling merchant-owned tables e.g. dashboard_configs #741).
--
-- alert_rules        — per-merchant threshold rules compiled from in-code
--                      templates; edge-triggered state (fired_at/cleared_at) so a
--                      rule fires once on crossing and stays quiet while active.
-- merchant_webhooks  — operator-configured OUTBOUND alert sinks (generic/discord/
--                      slack). Distinct from the /v1/merchants/{m}/webhooks/{p}
--                      INBOUND provider-webhook ingestion surface.
-- merchant_notifications — the in_app store, MERCHANT-operator-facing (the console
--                      bell). Distinct from the customer-facing notification_queue.

-- ============================================================================
-- alert_rules
-- ============================================================================
CREATE TABLE openrails.alert_rules (
    id uuid DEFAULT uuidv7() NOT NULL,
    merchant_id uuid NOT NULL,
    name text NOT NULL DEFAULT '',
    template text NOT NULL,
    params jsonb NOT NULL DEFAULT '{}'::jsonb,
    severity text NOT NULL DEFAULT 'warning',
    channels jsonb NOT NULL DEFAULT '[]'::jsonb,
    enabled boolean NOT NULL DEFAULT true,
    -- Edge-triggered state: fired_at is set on the crossing that opened the
    -- current active alert (NULL = not firing); cleared_at records the last
    -- recrossing back under threshold. last_evaluated_at gates digest cadence.
    fired_at timestamp with time zone,
    cleared_at timestamp with time zone,
    last_evaluated_at timestamp with time zone,
    last_value double precision,
    last_detail jsonb,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT alert_rules_severity_check CHECK ((severity = ANY (ARRAY['warning'::text, 'critical'::text])))
);

COMMENT ON TABLE openrails.alert_rules IS '#736 per-merchant metric threshold rules. template + params compile to a #733 metrics query the evaluator runs on a slow tick; fired_at/cleared_at are edge-triggered state (fire once on crossing, clear on recrossing).';
COMMENT ON COLUMN openrails.alert_rules.channels IS 'ordered channel refs: [{"type":"in_app"}|{"type":"email"}|{"type":"webhook","webhook_id":"<uuid>"}].';
COMMENT ON COLUMN openrails.alert_rules.fired_at IS 'set when the current active alert opened (NULL = not firing); the evaluator never re-fires while non-NULL.';

ALTER TABLE ONLY openrails.alert_rules
    ADD CONSTRAINT alert_rules_pkey PRIMARY KEY (id);

ALTER TABLE ONLY openrails.alert_rules
    ADD CONSTRAINT alert_rules_merchant_fk FOREIGN KEY (merchant_id) REFERENCES openrails.merchants(id) ON DELETE RESTRICT;

-- Evaluator's "armed merchants" lookup: DISTINCT merchant_id WHERE enabled.
-- Partial index so the scan cost scales with ACTIVITY (rules on file), never
-- with the merchant directory size (#719 only-run-what's-armed).
CREATE INDEX alert_rules_enabled_merchant_idx ON openrails.alert_rules (merchant_id) WHERE enabled;
CREATE INDEX alert_rules_merchant_idx ON openrails.alert_rules (merchant_id);

ALTER TABLE openrails.alert_rules ENABLE ROW LEVEL SECURITY;
ALTER TABLE ONLY openrails.alert_rules FORCE ROW LEVEL SECURITY;
CREATE POLICY merchant_isolation ON openrails.alert_rules USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));
GRANT SELECT,INSERT,UPDATE,DELETE ON TABLE openrails.alert_rules TO openrails_app;

-- ============================================================================
-- merchant_webhooks
-- ============================================================================
CREATE TABLE openrails.merchant_webhooks (
    id uuid DEFAULT uuidv7() NOT NULL,
    merchant_id uuid NOT NULL,
    name text NOT NULL DEFAULT '',
    url text NOT NULL,
    format text NOT NULL DEFAULT 'generic',
    enabled boolean NOT NULL DEFAULT true,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    CONSTRAINT merchant_webhooks_format_check CHECK ((format = ANY (ARRAY['generic'::text, 'discord'::text, 'slack'::text])))
);

COMMENT ON TABLE openrails.merchant_webhooks IS '#736 operator-configured OUTBOUND alert sinks. format shapes the POST body: generic=our alert JSON, discord={content}, slack={text}. NOT the inbound provider-webhook ingestion surface.';

ALTER TABLE ONLY openrails.merchant_webhooks
    ADD CONSTRAINT merchant_webhooks_pkey PRIMARY KEY (id);
ALTER TABLE ONLY openrails.merchant_webhooks
    ADD CONSTRAINT merchant_webhooks_merchant_fk FOREIGN KEY (merchant_id) REFERENCES openrails.merchants(id) ON DELETE RESTRICT;
CREATE INDEX merchant_webhooks_merchant_idx ON openrails.merchant_webhooks (merchant_id);

ALTER TABLE openrails.merchant_webhooks ENABLE ROW LEVEL SECURITY;
ALTER TABLE ONLY openrails.merchant_webhooks FORCE ROW LEVEL SECURITY;
CREATE POLICY merchant_isolation ON openrails.merchant_webhooks USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));
GRANT SELECT,INSERT,UPDATE,DELETE ON TABLE openrails.merchant_webhooks TO openrails_app;

-- ============================================================================
-- merchant_notifications  (the in_app bell store)
-- ============================================================================
CREATE TABLE openrails.merchant_notifications (
    id uuid DEFAULT uuidv7() NOT NULL,
    merchant_id uuid NOT NULL,
    severity text NOT NULL DEFAULT 'warning',
    title text NOT NULL,
    body text NOT NULL DEFAULT '',
    link text NOT NULL DEFAULT '',
    rule_id uuid,
    data jsonb,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    read_at timestamp with time zone
);

COMMENT ON TABLE openrails.merchant_notifications IS '#736 MERCHANT-operator-facing in_app alert store (console bell). rule_id references the source alert_rules row informationally (no FK: notifications outlive rule deletion).';

ALTER TABLE ONLY openrails.merchant_notifications
    ADD CONSTRAINT merchant_notifications_pkey PRIMARY KEY (id);
ALTER TABLE ONLY openrails.merchant_notifications
    ADD CONSTRAINT merchant_notifications_merchant_fk FOREIGN KEY (merchant_id) REFERENCES openrails.merchants(id) ON DELETE RESTRICT;

-- Bell query: unread badge + list by (merchant, read_at, recency).
CREATE INDEX merchant_notifications_bell_idx ON openrails.merchant_notifications (merchant_id, read_at, created_at DESC);

ALTER TABLE openrails.merchant_notifications ENABLE ROW LEVEL SECURITY;
ALTER TABLE ONLY openrails.merchant_notifications FORCE ROW LEVEL SECURITY;
CREATE POLICY merchant_isolation ON openrails.merchant_notifications USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));
GRANT SELECT,INSERT,UPDATE,DELETE ON TABLE openrails.merchant_notifications TO openrails_app;
