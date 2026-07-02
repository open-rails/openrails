-- #678: webhook dedup TRUTH. A row = this event's effects are durably applied;
-- Redis keeps only the pending-lease coordination and a completed-key cache, so
-- a Redis flush degrades to slower checks, never to replayed effects. Key
-- mirrors the Redis derivation: op = webhook.<rail>.<event_type>, event_id =
-- the rail's event/transaction id. Rows are written by ProcessWebhook (write-
-- after) or by the handler's own effect tx (MarkWebhookProcessedInTx).
CREATE TABLE openrails.webhook_events (
    merchant_id uuid NOT NULL,
    rail text NOT NULL,
    op text NOT NULL,
    event_id text NOT NULL,
    status text DEFAULT 'completed' NOT NULL,
    created_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    completed_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    CONSTRAINT webhook_events_pkey PRIMARY KEY (merchant_id, op, event_id),
    CONSTRAINT webhook_events_status_completed CHECK (status = 'completed')
);

COMMENT ON TABLE openrails.webhook_events IS '#678 webhook dedup truth: one row per applied webhook event (merchant, op, event_id). Pending/lease state stays in Redis (coordination, not truth); only completed marks live here.';
COMMENT ON COLUMN openrails.webhook_events.op IS 'Dedup operation key, webhook.<rail>.<event_type> — matches the Redis key derivation.';
COMMENT ON COLUMN openrails.webhook_events.status IS 'Always completed: pending coordination is Redis-only; a row here means effects are durably applied.';

-- Retention sweep scan (delete completed marks older than the cache TTL).
CREATE INDEX idx_webhook_events_completed_at ON openrails.webhook_events (completed_at);

ALTER TABLE openrails.webhook_events FORCE ROW LEVEL SECURITY;
ALTER TABLE openrails.webhook_events ENABLE ROW LEVEL SECURITY;
CREATE POLICY merchant_isolation ON openrails.webhook_events
    USING (merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)
    WITH CHECK (merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid);
GRANT SELECT,INSERT,DELETE ON TABLE openrails.webhook_events TO openrails_app;

ALTER TABLE ONLY openrails.webhook_events
    ADD CONSTRAINT webhook_events_merchant_fk FOREIGN KEY (merchant_id) REFERENCES openrails.merchants(id) ON DELETE RESTRICT;
