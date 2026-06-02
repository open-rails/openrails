-- Down migration for 053 — billing reconciliation events (issue #243).
DROP TABLE IF EXISTS billing.reconciliation_events;
