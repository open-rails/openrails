-- Durable webhook idempotency backup.
-- This table preserves dedupe state across Redis outages/restarts and worker restarts.
-- Records are intentionally retained in Postgres (no short TTL).

CREATE TABLE IF NOT EXISTS billing.webhook_idempotency_backup (
    operation TEXT NOT NULL,
    idempotency_key TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('pending', 'success', 'failed')),
    payload JSONB,
    error TEXT,
    first_seen_at TIMESTAMPTZ NOT NULL DEFAULT current_timestamp,
    last_seen_at TIMESTAMPTZ NOT NULL DEFAULT current_timestamp,
    created_at TIMESTAMPTZ NOT NULL DEFAULT current_timestamp,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT current_timestamp,
    PRIMARY KEY (operation, idempotency_key)
);

CREATE INDEX IF NOT EXISTS idx_webhook_idempotency_backup_status
    ON billing.webhook_idempotency_backup(status);

CREATE INDEX IF NOT EXISTS idx_webhook_idempotency_backup_last_seen_at
    ON billing.webhook_idempotency_backup(last_seen_at);

