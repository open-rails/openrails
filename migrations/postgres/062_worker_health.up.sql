-- #689: per-worker health bookkeeping — one row per registered River worker kind,
-- upserted by the worker-health middleware on every job completion and seeded at
-- registration by the health-check worker.
--
-- RLS POSTURE: worker health is OPERATOR-level, global to the deployment — like
-- openrails.merchants (control-plane directory) it carries no merchant_id and NO
-- RLS policy, so bare worker contexts (no app.merchant_id GUC) can read/write it.
CREATE TABLE openrails.worker_health (
    worker_kind text PRIMARY KEY,
    registered_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL,
    expected_period_seconds bigint,
    last_success_at timestamp with time zone,
    last_error_at timestamp with time zone,
    last_error text,
    consecutive_failures integer DEFAULT 0 NOT NULL,
    last_alerted_at timestamp with time zone,
    updated_at timestamp with time zone DEFAULT CURRENT_TIMESTAMP NOT NULL
);

COMMENT ON TABLE openrails.worker_health IS '#689 per-River-worker-kind health: last success/error + failure streak, written by the worker middleware. Operator-global control-plane table (no merchant scope, no RLS — see merchants).';
COMMENT ON COLUMN openrails.worker_health.registered_at IS 'First time this kind was seeded (deploy that introduced it) — anchors the never-succeeded-since-deploy alert.';
COMMENT ON COLUMN openrails.worker_health.expected_period_seconds IS 'Declared periodic cadence captured at registration; NULL/0 = on-demand kind (no staleness alerting).';
COMMENT ON COLUMN openrails.worker_health.last_error IS 'Most recent work error, truncated by the writer.';
COMMENT ON COLUMN openrails.worker_health.last_alerted_at IS 'When the health checker last raised a repair alert for this kind (dedup/re-alert pacing).';

GRANT SELECT,INSERT,DELETE,UPDATE ON TABLE openrails.worker_health TO openrails_app;
