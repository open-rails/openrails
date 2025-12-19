SET lock_timeout = '10s';
SET statement_timeout = '300s';

CREATE UNIQUE INDEX IF NOT EXISTS idx_webhook_events_processor_event_id_unique
  ON billing.webhook_events(processor, event_id)
  WHERE event_id IS NOT NULL;
